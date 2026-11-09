package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B8 review (pr61) F-1 follow-up. The collector raises PodPendingLong only for a pod
// with NO image error. So a PodPendingLong diagnosis that reaches the reconciler is,
// by construction, for a pod whose image error is gone this cycle. Folding it into
// the open image-error incident (imageErrorCovering -> foldIntoImageError) marks the
// image-error incident's signal active, so it never resolves, and the pod's real
// reason for staying Pending (an init container that never finishes, a volume that
// never attaches) gets neither its own incident nor a proposal for as long as the
// pod is Pending.
func TestReview_AFixedImageDoesNotKeepTheImageErrorIncidentAliveOrHideTheNextFault(t *testing.T) {
	e := newRecurrenceEnv(t, noiseClient(t, recurrencePersonaObject()))
	e.setImageError()
	e.cycle()
	imageIncident := e.onlyOpen()
	require.Equal(t, "ImagePullBackOff", imageIncident.Spec.Detection.Signal)

	// The image is fixed and pulls. The pod is still Pending for a different reason, so the
	// collector (no image error on the pod now) raises PodPendingLong, cycle after cycle.
	e.setPendingLong()
	for range 5 {
		e.cycle()
	}

	stillImageError := false
	pendingIncident := false
	for _, im := range e.open() {
		switch im.Spec.Detection.Signal {
		case "ImagePullBackOff":
			stillImageError = true
		case "PodPendingLong":
			pendingIncident = true
		}
	}
	assert.False(t, stillImageError && !pendingIncident,
		"the image error is gone and a different fault holds the pod Pending, yet the only open incident is still the image error (open: %s)",
		incidentSignals(e.open()))
}
