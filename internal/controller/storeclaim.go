package controller

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/willuhmjs/backshots/api/v1alpha1"
)

// A pod's inline NFS volume cannot carry mount options — only a PersistentVolume can — so a store
// that pins mountOptions (nfsvers, hard, timeo: the knobs that make or break NAS reliability) gets
// a static PV + PVC pair provisioned per namespace, and movers mount that claim instead. The PV is
// only a pointer at the export: its reclaim policy is Delete, which removes the object, never NFS
// data (the in-tree NFS driver wipes nothing).

const storeClaimSize = "1Gi" // NFS ignores capacity; kubelet only checks PVC request <= PV size

// storeClaimName is the deterministic name of the PV/PVC pair one store's mount options get in one
// namespace. Every policy in the namespace shares it, so the export is mounted once per node.
func storeClaimName(namespace, store string) string {
	return "backshots-store-" + shortHash(namespace+"/"+store)
}

// ensureStoreClaim provisions the PV+PVC pair for a store with mountOptions, and adds p as one of
// the claim's owners. When no mountOptions are set it is a no-op: movers then mount the export
// inline, exactly as before. It reports whether the pair is ready to mount, plus a message for why
// it is not.
func ensureStoreClaim(ctx context.Context, c client.Client, reader client.Reader, scheme *runtime.Scheme, p *v1alpha1.BackupPolicy, store *v1alpha1.BackupStore) (bool, string, error) {
	opts := store.Spec.NFS.MountOptions
	if len(opts) == 0 {
		return true, "", nil
	}
	name := storeClaimName(p.Namespace, store.Name)

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: p.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			VolumeName:       name,
			StorageClassName: ptr.To(""),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storeClaimSize)},
			},
		},
	}
	if err := createOrAddOwner(ctx, c, reader, scheme, p, pvc, &corev1.PersistentVolumeClaim{}); err != nil {
		return false, "", err
	}

	var pv corev1.PersistentVolume
	err := reader.Get(ctx, types.NamespacedName{Name: name}, &pv)
	if apierrors.IsNotFound(err) {
		want := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"backshots.io/store": labelValue(store.Name)}},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(storeClaimSize)},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				MountOptions:                  opts,
				VolumeMode:                    ptr.To(corev1.PersistentVolumeFilesystem),
				StorageClassName:              "",
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					NFS: &corev1.NFSVolumeSource{Server: store.Spec.NFS.Server, Path: store.Spec.NFS.Path},
				},
				ClaimRef: &corev1.ObjectReference{Namespace: p.Namespace, Name: name},
			},
		}
		if err := c.Create(ctx, want); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, "", err
		}
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	// A released leftover (all policies of the store were gone, the claim vanished) is deleted so
	// the next reconcile recreates it bound to the new claim.
	if pv.Status.Phase == corev1.VolumeReleased {
		return false, fmt.Sprintf("recreating released volume %s for the new claim", name), client.IgnoreNotFound(c.Delete(ctx, &pv))
	}
	// spec.mountOptions is immutable on a bound PV: refuse to pretend otherwise.
	if !slices.Equal(pv.Spec.MountOptions, opts) {
		return false, fmt.Sprintf("persistentvolume %s still mounts with its original options; run `kubectl delete pv %s` to apply the store's new mountOptions", name, name), nil
	}
	return true, "", nil
}

// createOrAddOwner creates obj when absent, or adds p as a plain (non-controller) owner so several
// policies in one namespace can co-own a shared object; garbage collection then happens exactly
// when the last of them is deleted. Reads go through the uncached reader to keep these types out of
// the manager's cache.
func createOrAddOwner(ctx context.Context, c client.Client, reader client.Reader, scheme *runtime.Scheme, p *v1alpha1.BackupPolicy, obj, protoType client.Object) error {
	err := reader.Get(ctx, client.ObjectKeyFromObject(obj), protoType)
	if apierrors.IsNotFound(err) {
		if err := controllerutil.SetOwnerReference(p, obj, scheme); err != nil {
			return err
		}
		if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}
	if ownsRef(protoType, p.UID) {
		return nil
	}
	before := protoType.DeepCopyObject().(client.Object)
	if err := controllerutil.SetOwnerReference(p, protoType, scheme); err != nil {
		return err
	}
	if err := c.Patch(ctx, protoType, client.MergeFrom(before)); err != nil && !apierrors.IsConflict(err) {
		return err // a conflict just means another policy (or our own cache lag) got there first
	}
	return nil
}

// ownsRef reports whether uid is among obj's owner references (any controller flag).
func ownsRef(obj client.Object, uid types.UID) bool {
	for _, o := range obj.GetOwnerReferences() {
		if o.UID == uid {
			return true
		}
	}
	return false
}

// labelValue keeps a label value valid (63 chars, safe alphabet) whatever the store is named.
func labelValue(s string) string {
	if len(s) <= 63 && isLabelSafe(s) {
		return s
	}
	return "h" + shortHash(s)
}

func isLabelSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return s != ""
}
