package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/willuhmjs/heirloom/api/v1"
)

// ensureExecutorRBAC maintains the namespace-local identity a command-source mover needs to exec
// into app pods: the ServiceAccount heirloom-executor, a Role granting nothing but reading pods and
// creating pods/exec, and the RoleBinding joining them. The objects are shared by every command
// policy in the namespace, so they carry non-controller owner references from each policy that needs
// them and garbage-collect once the last one is gone.
func (r *PolicyReconciler) ensureExecutorRBAC(ctx context.Context, p *v1.BackupPolicy) error {
	if err := createOrAddOwner(ctx, r.Client, r.Reader, r.Scheme, p, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: ExecutorServiceAccount, Namespace: p.Namespace},
	}, &corev1.ServiceAccount{}); err != nil {
		return err
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: ExecutorServiceAccount, Namespace: p.Namespace},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
		},
	}
	if err := createOrAddOwner(ctx, r.Client, r.Reader, r.Scheme, p, role, &rbacv1.Role{}); err != nil {
		return err
	}
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: ExecutorServiceAccount, Namespace: p.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: ExecutorServiceAccount},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: ExecutorServiceAccount, Namespace: p.Namespace}},
	}
	return createOrAddOwner(ctx, r.Client, r.Reader, r.Scheme, p, rb, &rbacv1.RoleBinding{})
}
