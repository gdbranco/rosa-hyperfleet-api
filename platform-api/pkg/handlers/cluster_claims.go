package handlers

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"

	hyperfleetv1alpha1 "github.com/openshift-online/rosa-hyperfleet-api/api/v1alpha1"
	"github.com/openshift-online/rosa-hyperfleet-api/platform-api/pkg/clients/hyperfleetdb"
)

const claimedByClusterUIDLabel = "hyperfleet.io/claimed-by-cluster-uid"

func (h *ClusterHandler) resolveDNSReservation(ctx context.Context, accountID, reservationUID string) (*hyperfleetv1alpha1.DNSReservation, *APIError) {
	if reservationUID == "" {
		return nil, &ErrClusterCreateDNSReservationRequired
	}
	reservation, err := h.db.GetDNSReservationByUID(ctx, accountID, reservationUID)
	if err != nil {
		if hyperfleetdb.IsNotFound(err) {
			return nil, &ErrClusterCreateDNSReservationNotFound
		}
		h.logger.Error("failed to look up DNS reservation", "error", err, "account_id", accountID, "reservation_uid", reservationUID)
		return nil, &ErrClusterCreateFailed
	}
	if string(reservation.UID) != reservationUID {
		return nil, &ErrClusterCreateDNSReservationNotFound
	}
	if reservation.Status.Phase != hyperfleetv1alpha1.DNSReservationPhaseReady || reservation.Status.BaseDomain == "" {
		return nil, &ErrClusterCreateDNSReservationNotReady
	}
	if reservation.Labels[claimedByClusterUIDLabel] != "" {
		return nil, &ErrClusterCreateDNSReservationInUse
	}
	return reservation, nil
}

func (h *ClusterHandler) resolveOidcConfig(ctx context.Context, accountID, oidcConfigUID string) (*hyperfleetv1alpha1.OidcConfig, *APIError) {
	if oidcConfigUID == "" {
		return nil, nil
	}
	oidcConfig, err := h.db.GetOidcConfigByUID(ctx, accountID, oidcConfigUID)
	if err != nil {
		if hyperfleetdb.IsNotFound(err) {
			return nil, &ErrClusterCreateOidcConfigNotFound
		}
		h.logger.Error("failed to look up OIDC config", "error", err, "account_id", accountID, "oidc_config_uid", oidcConfigUID)
		return nil, &ErrClusterCreateOidcConfigLookupFailed
	}
	if string(oidcConfig.UID) != oidcConfigUID {
		return nil, &ErrClusterCreateOidcConfigNotFound
	}
	if !oidcConfigUsable(oidcConfig) {
		return nil, &ErrClusterCreateOidcConfigNotReady
	}
	if oidcConfig.Labels[claimedByClusterUIDLabel] != "" {
		apiErr := ErrClusterCreateOidcConfigInUse.WithReason(oidcConfig.Name)
		return nil, &apiErr
	}
	return oidcConfig, nil
}

func (h *ClusterHandler) claimDNSReservation(ctx context.Context, accountID, reservationUID, clusterUID string) *APIError {
	reservation, err := h.db.GetDNSReservationByUID(ctx, accountID, reservationUID)
	if err != nil {
		if hyperfleetdb.IsNotFound(err) {
			return &ErrClusterCreateDNSReservationInUse
		}
		h.logger.Error("failed to re-fetch DNS reservation for claim", "error", err, "reservation_uid", reservationUID)
		return &ErrClusterCreateFailed
	}
	if string(reservation.UID) != reservationUID {
		return &ErrClusterCreateDNSReservationInUse
	}
	if reservation.Labels[claimedByClusterUIDLabel] == clusterUID {
		return nil
	}
	if reservation.Status.Phase != hyperfleetv1alpha1.DNSReservationPhaseReady || reservation.Status.BaseDomain == "" {
		return &ErrClusterCreateDNSReservationNotReady
	}
	if reservation.Labels[claimedByClusterUIDLabel] != "" {
		return &ErrClusterCreateDNSReservationInUse
	}
	if reservation.Labels == nil {
		reservation.Labels = make(map[string]string)
	}
	reservation.Labels[claimedByClusterUIDLabel] = clusterUID
	if err := h.db.UpdateDNSReservation(ctx, reservation); err != nil {
		if hyperfleetdb.IsConflict(err) || hyperfleetdb.IsAlreadyExists(err) {
			return &ErrClusterCreateDNSReservationInUse
		}
		h.logger.Error("failed to claim DNS reservation", "error", err, "account_id", accountID, "reservation_uid", reservationUID, "cluster_uid", clusterUID)
		return &ErrClusterCreateFailed
	}
	return nil
}

func (h *ClusterHandler) claimOidcConfig(ctx context.Context, accountID, oidcConfigUID, clusterUID string) *APIError {
	oidcConfig, err := h.db.GetOidcConfigByUID(ctx, accountID, oidcConfigUID)
	if err != nil {
		if hyperfleetdb.IsNotFound(err) {
			apiErr := ErrClusterCreateOidcConfigInUse.WithReason(oidcConfigUID)
			return &apiErr
		}
		h.logger.Error("failed to re-fetch OIDC config for claim", "error", err, "oidc_config_uid", oidcConfigUID)
		return &ErrClusterCreateOidcConfigLookupFailed
	}
	if string(oidcConfig.UID) != oidcConfigUID {
		apiErr := ErrClusterCreateOidcConfigInUse.WithReason(oidcConfigUID)
		return &apiErr
	}
	if oidcConfig.Labels[claimedByClusterUIDLabel] == clusterUID {
		return nil
	}
	if !oidcConfigUsable(oidcConfig) {
		return &ErrClusterCreateOidcConfigNotReady
	}
	if oidcConfig.Labels[claimedByClusterUIDLabel] != "" {
		apiErr := ErrClusterCreateOidcConfigInUse.WithReason(oidcConfig.Name)
		return &apiErr
	}
	if oidcConfig.Labels == nil {
		oidcConfig.Labels = make(map[string]string)
	}
	oidcConfig.Labels[claimedByClusterUIDLabel] = clusterUID
	if err := h.db.UpdateOidcConfigObject(ctx, oidcConfig); err != nil {
		if hyperfleetdb.IsConflict(err) || hyperfleetdb.IsAlreadyExists(err) {
			apiErr := ErrClusterCreateOidcConfigInUse.WithReason(oidcConfig.Name)
			return &apiErr
		}
		h.logger.Error("failed to claim OIDC config", "error", err, "account_id", accountID, "oidc_config_uid", oidcConfigUID, "cluster_uid", clusterUID)
		return &ErrClusterCreateOidcConfigLookupFailed
	}
	return nil
}

func (h *ClusterHandler) releaseDNSReservationClaim(ctx context.Context, accountID, reservationUID, clusterUID string) error {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseClaimTimeout)
	defer cancel()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		reservation, err := h.db.GetDNSReservationByUID(releaseCtx, accountID, reservationUID)
		if err != nil {
			return clientIgnoreNotFound(err)
		}
		if reservation.Labels[claimedByClusterUIDLabel] != clusterUID {
			return nil
		}
		delete(reservation.Labels, claimedByClusterUIDLabel)
		return h.db.UpdateDNSReservation(releaseCtx, reservation)
	})
}

func oidcConfigUsable(oidcConfig *hyperfleetv1alpha1.OidcConfig) bool {
	if oidcConfig.Status.Phase == hyperfleetv1alpha1.OidcConfigPhaseError {
		return false
	}
	return oidcConfig.Spec.Type != hyperfleetv1alpha1.OidcConfigTypeUnmanaged ||
		oidcConfig.Status.Phase == hyperfleetv1alpha1.OidcConfigPhaseReady
}

func clientIgnoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
