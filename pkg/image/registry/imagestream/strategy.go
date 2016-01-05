package imagestream

import (
	"fmt"
	"strings"

	"github.com/golang/glog"
	kapi "k8s.io/kubernetes/pkg/api"
	kerrors "k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/auth/user"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/registry/generic"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/fielderrors"
	"k8s.io/kubernetes/pkg/util/sets"

	authorizationapi "github.com/openshift/origin/pkg/authorization/api"
	"github.com/openshift/origin/pkg/authorization/registry/subjectaccessreview"
	"github.com/openshift/origin/pkg/image/api"
	"github.com/openshift/origin/pkg/image/api/validation"
)

type ResourceGetter interface {
	Get(kapi.Context, string) (runtime.Object, error)
}

// Strategy implements behavior for ImageStreams.
type Strategy struct {
	runtime.ObjectTyper
	kapi.NameGenerator
	defaultRegistry   DefaultRegistry
	tagVerifier       *TagVerifier
	ImageStreamGetter ResourceGetter
}

// NewStrategy is the default logic that applies when creating and updating
// ImageStream objects via the REST API.
func NewStrategy(defaultRegistry DefaultRegistry, subjectAccessReviewClient subjectaccessreview.Registry) Strategy {
	return Strategy{
		ObjectTyper:     kapi.Scheme,
		NameGenerator:   kapi.SimpleNameGenerator,
		defaultRegistry: defaultRegistry,
		tagVerifier:     &TagVerifier{subjectAccessReviewClient},
	}
}

// NamespaceScoped is true for image streams.
func (s Strategy) NamespaceScoped() bool {
	return true
}

// PrepareForCreate clears fields that are not allowed to be set by end users on creation,
// and verifies the current user is authorized to access any image streams newly referenced
// in spec.tags.
func (s Strategy) PrepareForCreate(obj runtime.Object) {
	stream := obj.(*api.ImageStream)
	stream.Status = api.ImageStreamStatus{
		DockerImageRepository: s.dockerImageRepository(stream),
		Tags: make(map[string]api.TagEventList),
	}
	stream.Generation = 1
	for tag, ref := range stream.Spec.Tags {
		ref.Generation = &stream.Generation
		stream.Spec.Tags[tag] = ref
	}
}

// Validate validates a new image stream.
func (s Strategy) Validate(ctx kapi.Context, obj runtime.Object) fielderrors.ValidationErrorList {
	stream := obj.(*api.ImageStream)
	user, ok := kapi.UserFrom(ctx)
	if !ok {
		return fielderrors.ValidationErrorList{kerrors.NewForbidden("imageStream", stream.Name, fmt.Errorf("unable to update an ImageStream without a user on the context"))}
	}
	errs := s.tagVerifier.Verify(nil, stream, user)
	errs = append(errs, s.tagsChanged(nil, stream)...)
	errs = append(errs, validation.ValidateImageStream(stream)...)
	return errs
}

// AllowCreateOnUpdate is false for image streams.
func (s Strategy) AllowCreateOnUpdate() bool {
	return false
}

func (Strategy) AllowUnconditionalUpdate() bool {
	return false
}

// dockerImageRepository determines the docker image stream for stream.
// If stream.DockerImageRepository is set, that value is returned. Otherwise,
// if a default registry exists, the value returned is of the form
// <default registry>/<namespace>/<stream name>.
func (s Strategy) dockerImageRepository(stream *api.ImageStream) string {
	registry, ok := s.defaultRegistry.DefaultRegistry()
	if !ok {
		return stream.Spec.DockerImageRepository
	}

	if len(stream.Namespace) == 0 {
		stream.Namespace = kapi.NamespaceDefault
	}
	ref := api.DockerImageReference{
		Registry:  registry,
		Namespace: stream.Namespace,
		Name:      stream.Name,
	}
	return ref.String()
}

func parseFromReference(stream *api.ImageStream, from *kapi.ObjectReference) (string, string, error) {
	splitChar := ""
	refType := ""

	switch from.Kind {
	case "ImageStreamTag":
		splitChar = ":"
		refType = "tag"
	case "ImageStreamImage":
		splitChar = "@"
		refType = "id"
	default:
		return "", "", fmt.Errorf("invalid from.kind %q - only ImageStreamTag and ImageStreamImage are allowed", from.Kind)
	}

	parts := strings.Split(from.Name, splitChar)
	switch len(parts) {
	case 1:
		// <tag> or <id>
		return stream.Name, from.Name, nil
	case 2:
		// <stream>:<tag> or <stream>@<id>
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("invalid from.name %q - it must be of the form <%s> or <stream>%s<%s>", from.Name, refType, splitChar, refType)
	}
}

// tagsChanged updates stream.Status.Tags based on the old and new image stream.
// if the old stream is nil, all tags are considered additions.
func (s Strategy) tagsChanged(old, stream *api.ImageStream) fielderrors.ValidationErrorList {
	var errs fielderrors.ValidationErrorList

	oldTags := map[string]api.TagReference{}
	if old != nil && old.Spec.Tags != nil {
		oldTags = old.Spec.Tags
	}

	for tag, tagRef := range stream.Spec.Tags {
		if oldRef, ok := oldTags[tag]; ok && !tagRefChanged(oldRef, tagRef, stream.Namespace) {
			continue
		}

		if tagRef.From == nil {
			continue
		}

		glog.V(5).Infof("Detected changed tag %s in %s/%s", tag, stream.Namespace, stream.Name)

		generation := stream.Generation
		tagRef.Generation = &generation

		if tagRef.From.Kind == "DockerImage" && len(tagRef.From.Name) > 0 {
			if tagRef.Reference {
				event, err := tagReferenceToTagEvent(stream, tagRef, "")
				if err != nil {
					errs = append(errs, fielderrors.NewFieldInvalid(fmt.Sprintf("spec.tags[%s].from", tag), tagRef.From, err.Error()))
					continue
				}
				stream.Spec.Tags[tag] = tagRef
				api.AddTagEventToImageStream(stream, tag, *event)
			}
			continue
		}

		tagRefStreamName, tagOrID, err := parseFromReference(stream, tagRef.From)
		if err != nil {
			errs = append(errs, fielderrors.NewFieldInvalid(fmt.Sprintf("spec.tags[%s].from.name", tag), tagRef.From.Name, "must be of the form <tag>, <repo>:<tag>, <id>, or <repo>@<id>"))
			continue
		}

		streamRef := stream
		streamRefNamespace := tagRef.From.Namespace
		if len(streamRefNamespace) == 0 {
			streamRefNamespace = stream.Namespace
		}
		if streamRefNamespace != stream.Namespace || tagRefStreamName != stream.Name {
			obj, err := s.ImageStreamGetter.Get(kapi.WithNamespace(kapi.NewContext(), streamRefNamespace), tagRefStreamName)
			if err != nil {
				if kerrors.IsNotFound(err) {
					errs = append(errs, fielderrors.NewFieldNotFound(fmt.Sprintf("spec.tags[%s].from.name", tag), tagRef.From.Name))
				} else {
					errs = append(errs, fielderrors.NewFieldInvalid(fmt.Sprintf("spec.tags[%s].from.name", tag), tagRef.From.Name, fmt.Sprintf("unable to retrieve image stream: %v", err)))
				}
				continue
			}

			streamRef = obj.(*api.ImageStream)
		}

		event, err := tagReferenceToTagEvent(streamRef, tagRef, tagOrID)
		if err != nil {
			errs = append(errs, fielderrors.NewFieldInvalid(fmt.Sprintf("spec.tags[%s].from.name", tag), tagRef.From.Name, fmt.Sprintf("error generating tag event: %v", err)))
			continue
		}

		if event == nil {
			// referenced tag or ID doesn't exist, which is ok
			continue
		}

		stream.Spec.Tags[tag] = tagRef
		api.AddTagEventToImageStream(stream, tag, *event)
	}

	api.UpdateChangedTrackingTags(stream, old)

	// use a consistent timestamp on creation
	if old == nil && !stream.CreationTimestamp.IsZero() {
		for tag, list := range stream.Status.Tags {
			for _, event := range list.Items {
				event.Created = stream.CreationTimestamp
			}
			stream.Status.Tags[tag] = list
		}
	}

	return errs
}

func tagReferenceToTagEvent(stream *api.ImageStream, tagRef api.TagReference, tagOrID string) (*api.TagEvent, error) {
	var (
		event *api.TagEvent
		err   error
	)
	switch tagRef.From.Kind {
	case "DockerImage":
		event = &api.TagEvent{
			Created:              unversioned.Now(),
			DockerImageReference: tagRef.From.Name,
		}

	case "ImageStreamImage":
		event, err = api.ResolveImageID(stream, tagOrID)
	case "ImageStreamTag":
		event, err = api.LatestTaggedImage(stream, tagOrID), nil
	default:
		err = fmt.Errorf("invalid from.kind %q: it must be DockerImage, ImageStreamImage or ImageStreamTag", tagRef.From.Kind)
	}
	if err != nil {
		return nil, err
	}
	if event != nil && tagRef.Generation != nil {
		event.Generation = *tagRef.Generation
	}
	return event, nil
}

// tagRefChanged returns true if the tag ref changed between two spec updates.
func tagRefChanged(old, next api.TagReference, streamNamespace string) bool {
	if next.From == nil {
		// both fields in next are empty
		return false
	}
	if len(next.From.Kind) == 0 && len(next.From.Name) == 0 {
		// invalid
		return false
	}
	oldFrom := old.From
	if oldFrom == nil {
		oldFrom = &kapi.ObjectReference{}
	}
	oldNamespace := oldFrom.Namespace
	if len(oldNamespace) == 0 {
		oldNamespace = streamNamespace
	}
	nextNamespace := next.From.Namespace
	if len(nextNamespace) == 0 {
		nextNamespace = streamNamespace
	}
	if oldNamespace != nextNamespace {
		return true
	}
	if oldFrom.Name != next.From.Name {
		return true
	}
	return tagRefGenerationChanged(old, next)
}

// tagRefGenerationChanged returns true if and only the values were set and the new generation
// is at zero.
func tagRefGenerationChanged(old, next api.TagReference) bool {
	switch {
	case old.Generation != nil && next.Generation != nil:
		if *old.Generation == *next.Generation {
			return false
		}
		if *next.Generation == 0 {
			return true
		}
		return false
	default:
		return false
	}
}

func tagEventChanged(old, next api.TagEvent) bool {
	return old.Image != next.Image || old.DockerImageReference != next.DockerImageReference || old.Generation > next.Generation
}

// updateSpecTagGenerationsForUpdate ensures that new spec updates always have a generation set, and that the value
// cannot be updated by an end user (except by setting generation 0).
func updateSpecTagGenerationsForUpdate(stream, oldStream *api.ImageStream) {
	for tag, ref := range stream.Spec.Tags {
		if ref.Generation != nil && *ref.Generation == 0 {
			continue
		}
		if oldRef, ok := oldStream.Spec.Tags[tag]; ok {
			ref.Generation = oldRef.Generation
			stream.Spec.Tags[tag] = ref
		}
	}
}

// ensureSpecTagGenerationsAreSet ensures that all spec tags have a generation set to either 0 or the
// current stream value.
func ensureSpecTagGenerationsAreSet(stream, oldStream *api.ImageStream) {
	oldTags := map[string]api.TagReference{}
	if oldStream != nil && oldStream.Spec.Tags != nil {
		oldTags = oldStream.Spec.Tags
	}

	// set the generation for any spec tags that have changed, are nil, or are zero
	for tag, ref := range stream.Spec.Tags {
		if oldRef, ok := oldTags[tag]; !ok || tagRefChanged(oldRef, ref, stream.Namespace) {
			ref.Generation = nil
		}

		if ref.Generation != nil && *ref.Generation != 0 {
			continue
		}
		ref.Generation = &stream.Generation
		stream.Spec.Tags[tag] = ref
	}
}

// updateObservedGenerationForStatusUpdate ensures every status item has a generation set.
func updateObservedGenerationForStatusUpdate(stream, oldStream *api.ImageStream) {
	for tag, newer := range stream.Status.Tags {
		if len(newer.Items) == 0 || newer.Items[0].Generation != 0 {
			// generation is set, continue
			continue
		}

		older := oldStream.Status.Tags[tag]
		if len(older.Items) == 0 || !tagEventChanged(older.Items[0], newer.Items[0]) {
			// if the tag wasn't changed by the status update
			newer.Items[0].Generation = stream.Generation
			stream.Status.Tags[tag] = newer
			continue
		}

		spec, ok := stream.Spec.Tags[tag]
		if !ok || spec.Generation == nil {
			// if the spec tag has no generation
			newer.Items[0].Generation = stream.Generation
			stream.Status.Tags[tag] = newer
			continue
		}

		// set the status tag from the spec tag generation
		newer.Items[0].Generation = *spec.Generation
		stream.Status.Tags[tag] = newer
	}
}

type TagVerifier struct {
	subjectAccessReviewClient subjectaccessreview.Registry
}

func (v *TagVerifier) Verify(old, stream *api.ImageStream, user user.Info) fielderrors.ValidationErrorList {
	var errors fielderrors.ValidationErrorList
	oldTags := map[string]api.TagReference{}
	if old != nil && old.Spec.Tags != nil {
		oldTags = old.Spec.Tags
	}
	for tag, tagRef := range stream.Spec.Tags {
		if tagRef.From == nil {
			continue
		}
		if len(tagRef.From.Namespace) == 0 {
			continue
		}
		if stream.Namespace == tagRef.From.Namespace {
			continue
		}
		if oldRef, ok := oldTags[tag]; ok && !tagRefChanged(oldRef, tagRef, stream.Namespace) {
			continue
		}

		streamName, _, err := parseFromReference(stream, tagRef.From)
		if err != nil {
			errors = append(errors, fielderrors.NewFieldInvalid(fmt.Sprintf("spec.tags[%s].from.name", tag), tagRef.From.Name, "must be of the form <tag>, <repo>:<tag>, <id>, or <repo>@<id>"))
			continue
		}

		subjectAccessReview := authorizationapi.SubjectAccessReview{
			Action: authorizationapi.AuthorizationAttributes{
				Verb:         "get",
				Resource:     "imagestreams",
				ResourceName: streamName,
			},
			User:   user.GetName(),
			Groups: sets.NewString(user.GetGroups()...),
		}
		ctx := kapi.WithNamespace(kapi.NewContext(), tagRef.From.Namespace)
		glog.V(4).Infof("Performing SubjectAccessReview for user=%s, groups=%v to %s/%s", user.GetName(), user.GetGroups(), tagRef.From.Namespace, streamName)
		resp, err := v.subjectAccessReviewClient.CreateSubjectAccessReview(ctx, &subjectAccessReview)
		if err != nil || resp == nil || (resp != nil && !resp.Allowed) {
			errors = append(errors, fielderrors.NewFieldForbidden(fmt.Sprintf("spec.tags[%s].from", tag), fmt.Sprintf("%s/%s", tagRef.From.Namespace, streamName)))
			continue
		}
	}
	return errors
}

func (s Strategy) prepareForUpdate(obj, old runtime.Object, resetStatus bool) {
	oldStream := old.(*api.ImageStream)
	stream := obj.(*api.ImageStream)

	stream.Generation = oldStream.Generation
	if resetStatus {
		stream.Status = oldStream.Status
	}
	stream.Status.DockerImageRepository = s.dockerImageRepository(stream)

	// ensure that users cannot change spec tag generation to any value except 0
	updateSpecTagGenerationsForUpdate(stream, oldStream)

	// Any changes to the spec increment the generation number.
	//
	// TODO: Any changes to a part of the object that represents desired state (labels,
	// annotations etc) should also increment the generation.
	if !kapi.Semantic.DeepEqual(oldStream.Spec, stream.Spec) || stream.Generation == 0 {
		stream.Generation = oldStream.Generation + 1
	}

	// default spec tag generations afterwards (to avoid updating the generation for legacy objects)
	ensureSpecTagGenerationsAreSet(stream, oldStream)
}

func (s Strategy) PrepareForUpdate(obj, old runtime.Object) {
	s.prepareForUpdate(obj, old, true)
}

// ValidateUpdate is the default update validation for an end user.
func (s Strategy) ValidateUpdate(ctx kapi.Context, obj, old runtime.Object) fielderrors.ValidationErrorList {
	stream := obj.(*api.ImageStream)

	user, ok := kapi.UserFrom(ctx)
	if !ok {
		return fielderrors.ValidationErrorList{kerrors.NewForbidden("imageStream", stream.Name, fmt.Errorf("unable to update an ImageStream without a user on the context"))}
	}

	oldStream := old.(*api.ImageStream)

	errs := s.tagVerifier.Verify(oldStream, stream, user)
	errs = append(errs, s.tagsChanged(oldStream, stream)...)
	errs = append(errs, validation.ValidateImageStreamUpdate(stream, oldStream)...)
	return errs
}

// Decorate decorates stream.Status.DockerImageRepository using the logic from
// dockerImageRepository().
func (s Strategy) Decorate(obj runtime.Object) error {
	ir := obj.(*api.ImageStream)
	ir.Status.DockerImageRepository = s.dockerImageRepository(ir)
	return nil
}

type StatusStrategy struct {
	Strategy
}

// NewStatusStrategy creates a status update strategy around an existing stream
// strategy.
func NewStatusStrategy(strategy Strategy) StatusStrategy {
	return StatusStrategy{strategy}
}

func (StatusStrategy) PrepareForUpdate(obj, old runtime.Object) {
	oldStream := old.(*api.ImageStream)
	stream := obj.(*api.ImageStream)

	stream.Spec.Tags = oldStream.Spec.Tags
	stream.Spec.DockerImageRepository = oldStream.Spec.DockerImageRepository

	updateObservedGenerationForStatusUpdate(stream, oldStream)
}

func (StatusStrategy) ValidateUpdate(ctx kapi.Context, obj, old runtime.Object) fielderrors.ValidationErrorList {
	// TODO: merge valid fields after update
	return validation.ValidateImageStreamStatusUpdate(obj.(*api.ImageStream), old.(*api.ImageStream))
}

// MatchImageStream returns a generic matcher for a given label and field selector.
func MatchImageStream(label labels.Selector, field fields.Selector) generic.Matcher {
	return generic.MatcherFunc(func(obj runtime.Object) (bool, error) {
		ir, ok := obj.(*api.ImageStream)
		if !ok {
			return false, fmt.Errorf("not an ImageStream")
		}
		fields := api.ImageStreamToSelectableFields(ir)
		return label.Matches(labels.Set(ir.Labels)) && field.Matches(fields), nil
	})
}

// DefaultRegistry returns the default Docker registry (host or host:port), or false if it is not available.
type DefaultRegistry interface {
	DefaultRegistry() (string, bool)
}

// DefaultRegistryFunc implements DefaultRegistry for a simple function.
type DefaultRegistryFunc func() (string, bool)

// DefaultRegistry implements the DefaultRegistry interface for a function.
func (fn DefaultRegistryFunc) DefaultRegistry() (string, bool) {
	return fn()
}

// InternalStrategy implements behavior for updating both the spec and status
// of an image stream
type InternalStrategy struct {
	Strategy
}

// NewInternalStrategy creates an update strategy around an existing stream
// strategy.
func NewInternalStrategy(strategy Strategy) InternalStrategy {
	return InternalStrategy{strategy}
}

func (s InternalStrategy) PrepareForCreate(obj runtime.Object) {
	stream := obj.(*api.ImageStream)

	stream.Status.DockerImageRepository = s.dockerImageRepository(stream)
	stream.Generation = 1
	for tag, ref := range stream.Spec.Tags {
		ref.Generation = &stream.Generation
		stream.Spec.Tags[tag] = ref
	}
}

func (s InternalStrategy) PrepareForUpdate(obj, old runtime.Object) {
	s.prepareForUpdate(obj, old, false)
}
