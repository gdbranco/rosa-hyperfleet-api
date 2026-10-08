package handlers

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeClusterClaimableResource struct {
	requestedUID      string
	uid               string
	storedClaimedBy   string
	workingClaimedBy  string
	storedFinalizers  []string
	workingFinalizers []string
	ready             bool
	saveCalls         int
	onFirstSave       func(*fakeClusterClaimableResource) error
}

func (r *fakeClusterClaimableResource) LoadByUID(context.Context) error {
	r.workingClaimedBy = r.storedClaimedBy
	r.workingFinalizers = append([]string(nil), r.storedFinalizers...)
	return nil
}

func (r *fakeClusterClaimableResource) RequestedUID() string { return r.requestedUID }
func (r *fakeClusterClaimableResource) UID() string          { return r.uid }
func (r *fakeClusterClaimableResource) ClaimedByClusterUID() string {
	return r.workingClaimedBy
}
func (r *fakeClusterClaimableResource) SetClaimedByClusterUID(uid string) {
	r.workingClaimedBy = uid
}
func (r *fakeClusterClaimableResource) ReadyForClusterClaim() bool { return r.ready }
func (r *fakeClusterClaimableResource) Save(context.Context) error {
	r.saveCalls++
	if r.saveCalls == 1 && r.onFirstSave != nil {
		return r.onFirstSave(r)
	}
	r.storedClaimedBy = r.workingClaimedBy
	r.storedFinalizers = append([]string(nil), r.workingFinalizers...)
	return nil
}
func (r *fakeClusterClaimableResource) NotFoundAPIError() *APIError {
	return &APIError{Code: "CLAIM-NOT-FOUND"}
}
func (r *fakeClusterClaimableResource) NotReadyAPIError() *APIError {
	return &APIError{Code: "CLAIM-NOT-READY"}
}
func (r *fakeClusterClaimableResource) InUseAPIError() *APIError {
	return &APIError{Code: "CLAIM-IN-USE"}
}

func TestClaimResourceRetriesConflictAndPreservesConcurrentMetadata(t *testing.T) {
	resource := &fakeClusterClaimableResource{
		requestedUID: "resource-uid",
		uid:          "resource-uid",
		ready:        true,
		onFirstSave: func(r *fakeClusterClaimableResource) error {
			r.storedFinalizers = append(r.storedFinalizers, "controller-finalizer")
			return apierrors.NewConflict(schema.GroupResource{Group: "hyperfleet.io", Resource: "claimables"}, "resource", errors.New("concurrent finalizer update"))
		},
	}

	apiErr, err := claimResource(context.Background(), "cluster-uid", resource)
	if err != nil {
		t.Fatalf("claimResource returned error: %v", err)
	}
	if apiErr != nil {
		t.Fatalf("claimResource returned API error: %v", apiErr)
	}
	if resource.saveCalls != 2 {
		t.Fatalf("Save called %d times, want one conflict retry", resource.saveCalls)
	}
	if resource.storedClaimedBy != "cluster-uid" {
		t.Errorf("claimed-by-cluster-uid = %q, want cluster-uid", resource.storedClaimedBy)
	}
	if len(resource.storedFinalizers) != 1 || resource.storedFinalizers[0] != "controller-finalizer" {
		t.Error("concurrent finalizer update was lost during claim retry")
	}
}

func TestClaimResourceReturnsInUseOnlyAfterFreshRead(t *testing.T) {
	resource := &fakeClusterClaimableResource{
		requestedUID: "resource-uid",
		uid:          "resource-uid",
		ready:        true,
		onFirstSave: func(r *fakeClusterClaimableResource) error {
			r.storedClaimedBy = "other-cluster-uid"
			return apierrors.NewConflict(schema.GroupResource{Group: "hyperfleet.io", Resource: "claimables"}, "resource", errors.New("concurrent claim"))
		},
	}

	apiErr, err := claimResource(context.Background(), "cluster-uid", resource)
	if err != nil {
		t.Fatalf("claimResource returned error: %v", err)
	}
	if apiErr == nil || apiErr.Code != "CLAIM-IN-USE" {
		t.Fatalf("claimResource API error = %v, want CLAIM-IN-USE", apiErr)
	}
	if resource.saveCalls != 1 {
		t.Fatalf("Save called %d times, want no update after observing the competing claim", resource.saveCalls)
	}
	if resource.storedClaimedBy != "other-cluster-uid" {
		t.Errorf("competing claim = %q, want other-cluster-uid", resource.storedClaimedBy)
	}
}
