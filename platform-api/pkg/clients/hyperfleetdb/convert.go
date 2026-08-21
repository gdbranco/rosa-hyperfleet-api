package hyperfleetdb

import (
	"encoding/json"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	hyperfleetv1alpha1 "github.com/openshift-online/rosa-hyperfleet-api/api/v1alpha1"
	public "github.com/openshift-online/rosa-hyperfleet-api/api/v1alpha1/public"
	"github.com/openshift-online/rosa-hyperfleet-api/platform-api/pkg/conversion"
	v1alpha1conv "github.com/openshift-online/rosa-hyperfleet-api/platform-api/pkg/conversion/v1alpha1"
)

// MergeSpecJSON merges raw JSON into dst. Only fields present in the JSON
// overwrite dst; omitted fields are preserved. This avoids data loss from
// non-omitempty struct fields (e.g. HostedCluster, NodePool passthrough)
// that would serialize as empty objects if marshaled from a typed Go struct.
func MergeSpecJSON(dst any, specJSON []byte) error {
	if len(specJSON) == 0 {
		return nil
	}
	return json.Unmarshal(specJSON, dst)
}

// --- Cluster conversions ---

// PublicToInternalCluster converts public.Cluster to internal v1alpha1.Cluster
// for storage in FleetDB. Enriches with service-set fields (accountID, internalID).
// Modifies pub.ObjectMeta in-place to set namespace, UID, and labels.
func PublicToInternalCluster(pub *public.Cluster, accountID, clusterID string) *hyperfleetv1alpha1.Cluster {
	if pub == nil {
		return nil
	}

	enrichMetadata(&pub.ObjectMeta, clusterID, accountID)

	enrichment := &conversion.ServiceSetFields{
		AccountID:  accountID,
		InternalID: clusterID,
	}
	crdSpec := v1alpha1conv.UnprojectCluster(&pub.Spec, enrichment)

	return &hyperfleetv1alpha1.Cluster{
		TypeMeta:   pub.TypeMeta,
		ObjectMeta: pub.ObjectMeta,
		Spec:       *crdSpec,
	}
}

// InternalToPublicCluster converts internal v1alpha1.Cluster to public.Cluster
// for REST API responses. Filters service-set fields via JSON roundtrip projection.
// metadata.uid is overridden with the cluster UUID (from namespace) because FleetDB
// assigns its own UID on storage. SDK callers rely on string(cluster.UID) as the
// stable identifier for Get/WaitUntil, which the handler routes by cluster UUID.
func InternalToPublicCluster(cr *hyperfleetv1alpha1.Cluster) *public.Cluster {
	if cr == nil {
		return nil
	}
	pub := v1alpha1conv.ProjectCluster(cr)
	pub.UID = types.UID(clusterIDFromNamespace(cr.Namespace))
	return pub
}

// --- NodePool conversions ---

// PublicToInternalNodePool converts public.NodePool to internal v1alpha1.NodePool
// for storage in FleetDB. Enriches with service-set fields and syncs the top-level
// autoRepair and labels fields into the HyperShift passthrough for internal consistency.
// Modifies pub.ObjectMeta in-place to set namespace and labels.
func PublicToInternalNodePool(pub *public.NodePool, accountID, clusterID, internalPoolID string) *hyperfleetv1alpha1.NodePool {
	if pub == nil {
		return nil
	}

	enrichMetadata(&pub.ObjectMeta, clusterID, accountID)

	// Capture top-level user values before Unproject, which overlays ServiceSetFields
	// (some of which have no omitempty and can zero out fields like Labels).
	userAutoRepair := pub.Spec.AutoRepair
	userLabels := pub.Spec.Labels

	enrichment := &conversion.ServiceSetFields{
		AccountID:      accountID,
		InternalPoolID: internalPoolID,
	}
	crdSpec := v1alpha1conv.UnprojectNodePool(&pub.Spec, enrichment)

	// ServiceSetFields.Labels has no omitempty so the overlay can zero spec.Labels.
	// Restore the user-supplied labels so they're preserved in the stored CRD and
	// remain readable by ProjectNodePool on the read path.
	crdSpec.Labels = userLabels

	// Sync top-level fields into the HyperShift passthrough using the pre-Unproject
	// values; the operator owns management.autoRepair and nodeLabels and reconciles them,
	// but we mirror them here so the stored CRD is internally consistent from day one.
	syncNodePoolPassthrough(crdSpec, userAutoRepair, userLabels)

	return &hyperfleetv1alpha1.NodePool{
		TypeMeta:   pub.TypeMeta,
		ObjectMeta: pub.ObjectMeta,
		Spec:       *crdSpec,
	}
}

// InternalToPublicNodePool converts internal v1alpha1.NodePool to public.NodePool
// for REST API responses. Filters service-set fields via JSON roundtrip projection.
// metadata.uid is overridden with cr.Name because GetNodePool looks up by name, and
// SDK callers rely on string(np.UID) as the stable identifier for Get/WaitUntil.
func InternalToPublicNodePool(cr *hyperfleetv1alpha1.NodePool) *public.NodePool {
	if cr == nil {
		return nil
	}
	pub := v1alpha1conv.ProjectNodePool(cr)
	pub.UID = types.UID(cr.Name)
	return pub
}

// --- Helpers ---

// enrichMetadata sets K8s namespace, UID, and account label on meta in-place.
// Uses clusterNamespace() to ensure consistent "cluster-<uuid>" prefix format.
func enrichMetadata(meta *metav1.ObjectMeta, resourceID, accountID string) {
	meta.Namespace = clusterNamespace(resourceID)
	meta.UID = types.UID(resourceID)
	if meta.Labels == nil {
		meta.Labels = make(map[string]string)
	}
	meta.Labels["hyperfleet.io/account-id"] = accountID
}

// syncNodePoolPassthrough mirrors the top-level autoRepair and labels into the
// HyperShift passthrough for internal consistency. Called only during public→internal
// conversion; the operator reconciles these during its own sync loop.
// autoRepair and labels are passed explicitly because the ServiceSetFields overlay in
// UnprojectNodePool can zero out spec.Labels (no omitempty on ServiceSetFields.Labels).
func syncNodePoolPassthrough(spec *hyperfleetv1alpha1.NodePoolSpec, autoRepair *bool, labels map[string]string) {
	if spec == nil {
		return
	}

	// Default autoRepair to true when unset (matches operator behavior).
	if autoRepair != nil {
		spec.NodePool.Management.AutoRepair = *autoRepair
	} else {
		spec.NodePool.Management.AutoRepair = true
	}

	spec.NodePool.NodeLabels = labels
}

const clusterNSPrefix = "cluster-"

// clusterUUIDLen is the fixed length of a RFC 4122 UUID string (e.g. "4610b27e-8f77-4f4c-9661-c11b42e04dec").
const clusterUUIDLen = 36

// MaxClusterNameLen is the maximum allowed cluster name length.
// HyperShift creates a control plane namespace as "<hc-namespace>-<hc-name>",
// which expands to "cluster-<uuid>-<name>" and must fit within 63 characters (k8s namespace limit).
const MaxClusterNameLen = 63 - len(clusterNSPrefix) - clusterUUIDLen - len("-")

func clusterNamespace(clusterID string) string {
	return clusterNSPrefix + clusterID
}

func clusterIDFromNamespace(ns string) string {
	return strings.TrimPrefix(ns, clusterNSPrefix)
}

// ClusterIDFromNamespace extracts the cluster UUID from a K8s namespace string ("cluster-<uuid>").
// Exported for use by handlers that need to derive the cluster ID from a NodePool's namespace.
func ClusterIDFromNamespace(ns string) string {
	return clusterIDFromNamespace(ns)
}
