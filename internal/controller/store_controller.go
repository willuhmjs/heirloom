package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1 "github.com/willuhmjs/heirloom/api/v1"
)

// StoreReconciler keeps the BackupStore status (where it points, whether it is encrypted) current, so that
// dashboards and `kubectl get backupstores` can show it in one column whatever the protocol.
type StoreReconciler struct {
	client.Client
}

func (r *StoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named("backupstore").For(&v1.BackupStore{}).Complete(r)
}

// storeTarget is a one-line, URL-like description of where the store points.
func storeTarget(store *v1.BackupStore) string {
	switch {
	case store.Spec.SMB != nil:
		return fmt.Sprintf("smb://%s/%s", store.Spec.SMB.Server, store.Spec.SMB.Share)
	case store.Spec.NFS != nil:
		return fmt.Sprintf("nfs://%s%s", store.Spec.NFS.Server, store.Spec.NFS.Path)
	}
	return ""
}

func (r *StoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var store v1.BackupStore
	if err := r.Get(ctx, req.NamespacedName, &store); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	want := v1.BackupStoreStatus{Target: storeTarget(&store), Encrypted: storeEncrypted(&store)}
	if store.Status == want {
		return ctrl.Result{}, nil
	}
	store.Status = want
	if err := r.Status().Update(ctx, &store); err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}
