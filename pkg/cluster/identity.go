package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// IdentityOptions configure the ServiceAccount and its RBAC.
type IdentityOptions struct {
	// Config reaches the API server as admin.
	Config *rest.Config
	// Namespace is the run's namespace. Roles are bound there.
	Namespace string
	// Roles are bound in Namespace.
	Roles []rbacv1.Role
	// ClusterRoles are copied with a per-run name and bound cluster-wide.
	ClusterRoles []rbacv1.ClusterRole
}

// Identity is a ServiceAccount with bound roles and a bearer token. The run
// passes its Config to the proxy so the target talks to the API server as this
// account instead of as admin.
type Identity struct {
	config    *rest.Config
	client    kubernetes.Interface
	namespace string
	// clusterRoles are the copies Delete removes.
	clusterRoles []string
	// clusterRoleBindings are the bindings Delete removes.
	clusterRoleBindings []string
}

// saName is the ServiceAccount name in every run namespace.
const saName = "botbox-target"

// NewIdentity creates a ServiceAccount in the namespace, creates and binds the
// roles, requests a token and returns an Identity whose Config carries it. The
// caller must call Delete.
func NewIdentity(opts IdentityOptions) (*Identity, error) {
	if opts.Namespace == "" {
		return nil, errors.New("creating the target identity: a namespace is required")
	}
	if opts.Config == nil {
		return nil, errors.New("creating the target identity: a Config is required")
	}
	id := &Identity{namespace: opts.Namespace}
	if err := id.create(opts); err != nil {
		// Best-effort cleanup of what was created.
		_ = id.Delete(context.Background())
		return nil, err
	}
	return id, nil
}

func (id *Identity) create(opts IdentityOptions) error {
	client, err := kubernetes.NewForConfig(opts.Config)
	if err != nil {
		return fmt.Errorf("creating the target identity: %w", err)
	}
	id.client = client
	ctx := context.TODO()

	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: saName}}
	if _, err := client.CoreV1().ServiceAccounts(opts.Namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating the target ServiceAccount: %w", err)
	}

	for _, role := range opts.Roles {
		role.Namespace = opts.Namespace
		if _, err := client.RbacV1().Roles(opts.Namespace).Create(ctx, &role, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating Role %s: %w", role.Name, err)
		}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: opts.Namespace}},
			RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: role.Name},
		}
		if _, err := client.RbacV1().RoleBindings(opts.Namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating RoleBinding %s: %w", role.Name, err)
		}
	}

	for _, cr := range opts.ClusterRoles {
		// Per-run copy so that concurrent runs do not collide.
		copied := cr.DeepCopy()
		copied.Name = cr.Name + "-" + opts.Namespace
		copied.Labels = map[string]string{"botbox.dev/namespace": opts.Namespace}
		if _, err := client.RbacV1().ClusterRoles().Create(ctx, copied, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating ClusterRole %s: %w", copied.Name, err)
		}
		id.clusterRoles = append(id.clusterRoles, copied.Name)

		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name:   copied.Name,
				Labels: map[string]string{"botbox.dev/namespace": opts.Namespace},
			},
			Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: saName, Namespace: opts.Namespace}},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: copied.Name},
		}
		if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating ClusterRoleBinding %s: %w", copied.Name, err)
		}
		id.clusterRoleBindings = append(id.clusterRoleBindings, copied.Name)
	}

	token, err := id.requestToken(ctx, opts)
	if err != nil {
		return err
	}
	id.config = rest.CopyConfig(opts.Config)
	id.config.BearerToken = token
	// Clear cert-based auth so the token is used.
	id.config.CertData = nil
	id.config.CertFile = ""
	id.config.KeyData = nil
	id.config.KeyFile = ""
	id.config.Username = ""
	id.config.Password = ""
	return nil
}

// tokenLifetime is how long the SA token lasts. A run outlasting it would need
// a refresh, which no run does today.
const tokenLifetime = 1 * time.Hour

func (id *Identity) requestToken(ctx context.Context, opts IdentityOptions) (string, error) {
	seconds := int64(tokenLifetime.Seconds())
	request := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: &seconds,
		},
	}
	response, err := id.client.CoreV1().ServiceAccounts(opts.Namespace).
		CreateToken(ctx, saName, request, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("requesting a token for the target ServiceAccount: %w", err)
	}
	return response.Status.Token, nil
}

// Config returns a rest.Config that authenticates as the ServiceAccount.
func (id *Identity) Config() *rest.Config {
	return id.config
}

// Delete removes the cluster-scoped resources this identity created. The
// namespace-scoped resources go with the namespace.
func (id *Identity) Delete(ctx context.Context) error {
	if id.client == nil {
		return nil
	}
	var errs []error
	for _, name := range id.clusterRoleBindings {
		err := id.client.RbacV1().ClusterRoleBindings().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting ClusterRoleBinding %s: %w", name, err))
		}
	}
	for _, name := range id.clusterRoles {
		err := id.client.RbacV1().ClusterRoles().Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("deleting ClusterRole %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}
