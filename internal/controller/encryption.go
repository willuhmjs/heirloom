package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	v1 "github.com/willuhmjs/heirloom/api/v1"
	"github.com/willuhmjs/heirloom/internal/jobcfg"
)

// A store with spec.encryption encrypts every policy's restic repository. The operator holds one master
// password and derives a separate repository password per policy from it, so a Secret copied into a
// tenant's namespace only ever opens that tenant's own repository, never a neighbour's.

// storeEncrypted reports whether the store's repositories carry a password.
func storeEncrypted(store *v1.BackupStore) bool { return store.Spec.Encryption != nil }

// repoKeySecretName is the deterministic name of the Secret holding one policy's derived repository password.
func repoKeySecretName(namespace, policy string) string {
	return "heirloom-repo-key-" + shortHash(namespace+"/"+policy)
}

// DerivePassword is the repository password of policy namespace/policy under master. It is documented
// (docs/RESTORE-RUNBOOKS.md) so a repository can be opened by hand with the restic binary.
func DerivePassword(master []byte, namespace, policy string) string {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("heirloom/v1/" + namespace + "/" + policy))
	return hex.EncodeToString(mac.Sum(nil))
}

// ensureRepoKey makes sure the policy's derived-password Secret exists in its namespace, owned by the policy
// so it is garbage-collected with it. It is create-only: the value is a pure function of the master password
// and the policy's identity, and changing the master is not supported, so an existing Secret is never
// rewritten. A store without encryption is a no-op. The reader is uncached to keep Secrets out of the cache.
func ensureRepoKey(ctx context.Context, c client.Client, reader client.Reader, scheme *runtime.Scheme, p *v1.BackupPolicy, store *v1.BackupStore) (bool, string, error) {
	enc := store.Spec.Encryption
	if enc == nil {
		return true, "", nil
	}
	ref := enc.PasswordSecretRef
	key := ref.Key
	if key == "" {
		key = jobcfg.KeySecretKey
	}
	var master corev1.Secret
	err := reader.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &master)
	switch {
	case apierrors.IsNotFound(err):
		return false, fmt.Sprintf("store %s encryption: secret %s/%s not found", store.Name, ref.Namespace, ref.Name), nil
	case apierrors.IsForbidden(err):
		return false, fmt.Sprintf("store %s encryption: the operator may only read secrets in its own namespace; move %s/%s there", store.Name, ref.Namespace, ref.Name), nil
	case err != nil:
		return false, "", err
	}
	pw := master.Data[key]
	if len(pw) == 0 {
		return false, fmt.Sprintf("store %s encryption: secret %s/%s has no key %q (or it is empty)", store.Name, ref.Namespace, ref.Name, key), nil
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: repoKeySecretName(p.Namespace, p.Name), Namespace: p.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{jobcfg.KeySecretKey: DerivePassword(pw, p.Namespace, p.Name)},
	}
	if err := controllerutil.SetControllerReference(p, sec, scheme); err != nil {
		return false, "", err
	}
	if err := c.Create(ctx, sec); err != nil && !apierrors.IsAlreadyExists(err) {
		return false, "", err
	}
	return true, "", nil
}
