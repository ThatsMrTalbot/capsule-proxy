package webserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	capsulev1alpha1 "github.com/clastix/capsule/api/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/gorilla/handlers"
	"github.com/gorilla/mux"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/clastix/capsule-proxy/internal/modules"
	moderrors "github.com/clastix/capsule-proxy/internal/modules/errors"
	"github.com/clastix/capsule-proxy/internal/modules/ingressclass"
	"github.com/clastix/capsule-proxy/internal/modules/namespace"
	"github.com/clastix/capsule-proxy/internal/modules/node"
	"github.com/clastix/capsule-proxy/internal/modules/storageclass"
	"github.com/clastix/capsule-proxy/internal/options"
	req "github.com/clastix/capsule-proxy/internal/request"
	serverr "github.com/clastix/capsule-proxy/internal/webserver/errors"
	"github.com/clastix/capsule-proxy/internal/webserver/middleware"
)

func NewKubeFilter(opts options.ListenerOpts, srv options.ServerOptions) (Filter, error) {
	reverseProxy := httputil.NewSingleHostReverseProxy(opts.KubernetesControlPlaneURL())
	reverseProxy.FlushInterval = time.Millisecond * 100

	reverseProxyTransport, err := opts.ReverseProxyTransport()
	if err != nil {
		return nil, errors.Wrap(err, "cannot create transport for reverse proxy")
	}

	reverseProxy.Transport = reverseProxyTransport

	return &kubeFilter{
		capsuleUserGroup:   opts.UserGroupName(),
		reverseProxy:       reverseProxy,
		bearerToken:        opts.BearerToken(),
		usernameClaimField: opts.PreferredUsernameClaim(),
		serverOptions:      srv,
		log:                ctrl.Log.WithName("namespace_filter"),
	}, nil
}

type kubeFilter struct {
	capsuleUserGroup   string
	reverseProxy       *httputil.ReverseProxy
	client             client.Client
	bearerToken        string
	usernameClaimField string
	serverOptions      options.ServerOptions
	log                logr.Logger
}

func (n *kubeFilter) LivenessProbe(req *http.Request) error {
	return nil
}

func (n *kubeFilter) ReadinessProbe(req *http.Request) (err error) {
	scheme := "http"
	clt := &http.Client{}

	if n.serverOptions.IsListeningTLS() {
		scheme = "https"
		clt = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					//nolint:gosec
					InsecureSkipVerify: true,
				},
			},
		}
	}

	url := fmt.Sprintf("%s://localhost:%d/_healthz", scheme, n.serverOptions.ListeningPort())

	var r *http.Request

	if r, err = http.NewRequestWithContext(context.Background(), "GET", url, nil); err != nil {
		return errors.Wrap(err, "cannot create request")
	}

	var resp *http.Response

	if resp, err = clt.Do(r); err != nil {
		return errors.Wrap(err, "cannot make local _healthz request")
	}

	defer func() {
		_ = resp.Body.Close()
	}()

	if sc := resp.StatusCode; sc != 200 {
		return fmt.Errorf("returned status code from _healthz is %d, expected 200", sc)
	}

	return nil
}

func (n *kubeFilter) InjectClient(client client.Client) error {
	n.client = client

	return nil
}

func (n kubeFilter) reverseProxyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request)

		n.log.V(5).Info("debugging request", "uri", request.RequestURI, "method", request.Method)
		n.reverseProxy.ServeHTTP(writer, request)
	})
}

// nolint:interfacer
func (n kubeFilter) handleRequest(request *http.Request, username string, selector labels.Selector) {
	q := request.URL.Query()
	if e := q.Get("labelSelector"); len(e) > 0 {
		n.log.V(4).Info("handling current labelSelector", "selector", e)

		if err := n.validateCapsuleLabel(e, username); err != nil {
			n.log.Error(err, "cannot validate Capsule label selector, ignoring it")
			panic(err)
		}

		v := strings.Join([]string{e, selector.String()}, ",")
		q.Set("labelSelector", v)
		n.log.V(4).Info("labelSelector updated", "selector", v)
	} else {
		q.Set("labelSelector", selector.String())
		n.log.V(4).Info("labelSelector added", "selector", selector.String())
	}

	n.log.V(4).Info("updating RawQuery", "query", q.Encode())
	request.URL.RawQuery = q.Encode()

	if len(n.bearerToken) > 0 {
		n.log.V(4).Info("Updating the token", "token", n.bearerToken)
		request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", n.bearerToken))
	}
}

func (n kubeFilter) impersonateHandler(writer http.ResponseWriter, request *http.Request) {
	if !n.serverOptions.IsListeningTLS() {
		return
	}

	n.log.V(3).Info("running on TLS, need to check the certificate")

	if pc := request.TLS.PeerCertificates; len(pc) == 1 {
		hr := req.NewHTTP(request, n.usernameClaimField, n.client)

		var username string

		var groups []string

		var err error

		if username, groups, err = hr.GetUserAndGroups(); err != nil {
			serverr.HandleError(writer, err, "Cannot retrieve user and group from Request certificate")
		}

		n.log.V(4).Info("Impersonating for the current request", "username", username, "groups", groups, "token", n.bearerToken)

		if len(n.bearerToken) > 0 {
			request.Header.Set("Authorization", fmt.Sprintf("Bearer %s", n.bearerToken))
		}

		request.Header.Add("Impersonate-User", username)
		request.Header.Add("Impersonate-Group", strings.Join(groups, ","))
	}
}

func (n kubeFilter) registerModules(root *mux.Router) {
	modList := []modules.Module{
		namespace.List(n.client),
		node.List(n.client),
		node.Get(n.client),
		ingressclass.List(n.client),
		ingressclass.Get(n.client),
		storageclass.Get(n.client),
		storageclass.List(n.client),
	}
	for _, i := range modList {
		mod := i
		rp := root.Path(mod.Path())

		if m := mod.Methods(); len(m) > 0 {
			rp = rp.Methods(m...)
		}

		sr := rp.Subrouter()
		sr.Use(
			middleware.CheckJWTMiddleware(n.client, n.log),
			middleware.CheckUserInCapsuleGroupMiddleware(n.client, n.log, n.usernameClaimField, n.capsuleUserGroup, n.impersonateHandler),
		)
		sr.HandleFunc("", func(writer http.ResponseWriter, request *http.Request) {
			username, groups, _ := req.NewHTTP(request, n.usernameClaimField, n.client).GetUserAndGroups()
			tenantList, err := n.getTenantsForOwner(username, groups)
			if err != nil {
				serverr.HandleError(writer, err, "cannot list Tenant resources")
			}

			var selector labels.Selector
			selector, err = mod.Handle(tenantList, request)
			switch {
			case err != nil:
				var t moderrors.Error
				if errors.As(err, &t) {
					writer.Header().Set("content-type", "application/json")
					b, _ := json.Marshal(t.Status())
					_, _ = writer.Write(b)
					panic(err.Error())
				}
				serverr.HandleError(writer, err, err.Error())
			case selector == nil:
				// if there's no selector, let it pass to the
				n.impersonateHandler(writer, request)
			default:
				n.handleRequest(request, username, selector)
			}
		})
	}
}

func (n kubeFilter) Start(ctx context.Context) error {
	r := mux.NewRouter().StrictSlash(true)
	r.Use(handlers.RecoveryHandler())
	r.Path("/_healthz").Subrouter().HandleFunc("", func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(200)
		_, _ = writer.Write([]byte("ok"))
	})

	root := r.PathPrefix("").Subrouter()
	root.Use(n.reverseProxyMiddleware)
	n.registerModules(root)
	root.PathPrefix("/").HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		n.impersonateHandler(writer, request)
	})

	var srv *http.Server

	go func() {
		var err error

		addr := fmt.Sprintf("0.0.0.0:%d", n.serverOptions.ListeningPort())

		if n.serverOptions.IsListeningTLS() {
			tlsConfig := &tls.Config{
				MinVersion: tls.VersionTLS12,
				ClientCAs:  n.serverOptions.GetCertificateAuthorityPool(),
				ClientAuth: tls.VerifyClientCertIfGiven,
			}
			srv = &http.Server{
				Handler:   r,
				Addr:      addr,
				TLSConfig: tlsConfig,
			}
			err = srv.ListenAndServeTLS(n.serverOptions.TLSCertificatePath(), n.serverOptions.TLSCertificateKeyPath())
		} else {
			srv = &http.Server{
				Handler: r,
				Addr:    addr,
			}
			err = srv.ListenAndServe()
		}

		if err != nil {
			panic(err)
		}
	}()

	<-ctx.Done()

	return srv.Shutdown(ctx)
}

func (n *kubeFilter) getTenantsForOwner(username string, groups []string) (tenants *capsulev1alpha1.TenantList, err error) {
	ownedTenants, _, err := n.getTenantsForOwnerKind("User", username)
	if err != nil {
		return nil, fmt.Errorf("cannot get Tenants slice owned by Tenant Owner: %w", err)
	}
	// Find tenants belonging to a group
	for _, group := range groups {
		t, _, err := n.getTenantsForOwnerKind("Group", group)
		if err != nil {
			return nil, fmt.Errorf("cannot get Tenants slice owned by Tenant Owner: %w", err)
		}

		ownedTenants.Items = append(ownedTenants.Items, t.Items...)
	}

	return ownedTenants, nil
}

func (n kubeFilter) getTenantsForOwnerKind(ownerKind string, ownerName string) (tl *capsulev1alpha1.TenantList, tenants []string, err error) {
	tl = &capsulev1alpha1.TenantList{}

	f := client.MatchingFields{
		".spec.owner.ownerkind": fmt.Sprintf("%s:%s", ownerKind, ownerName),
	}
	if err := n.client.List(context.Background(), tl, f); err != nil {
		return nil, nil, fmt.Errorf("cannot retrieve Tenants list: %w", err)
	}

	for _, t := range tl.Items {
		tenants = append(tenants, t.GetName())
	}

	n.log.V(4).Info("Tenants list", "owner", ownerKind, "name", ownerName, "tenants", tenants)

	return
}

// We have to validate User requesting labels since we're changing the Authorization Bearer since the Tenant Owner
// does not have permission to list Namespaces: in case of filtering by non-owned namespaces, we have to return an
// error, otherwise everything is good.
func (n kubeFilter) validateCapsuleLabel(value, username string) error {
	p, err := labels.Parse(value)
	if err != nil {
		// we're ignoring this, API Server will deal with this
		return nil
	}

	capsuleLabel, err := capsulev1alpha1.GetTypeLabel(&capsulev1alpha1.Tenant{})
	if err != nil {
		return fmt.Errorf("cannot get Capsule Tenant label: %w", err)
	}

	r, selectable := p.Requirements()
	if !selectable {
		return nil
	}

	for _, i := range r {
		if i.Key() == capsuleLabel {
			_, tenants, _ := n.getTenantsForOwnerKind("User", username)
			// nolint:exhaustive
			switch i.Operator() {
			case selection.Exists:
				return fmt.Errorf("cannot return list of all Tenant namespaces")
			case selection.In:
				if ss := i.Values().Delete(tenants...); ss.Len() > 0 {
					tnts := strings.Join(ss.List(), ", ")

					return fmt.Errorf("cannot list Namespaces for the following Tenant(s): %s", tnts)
				}
			default:
				break
			}
		}
	}

	return nil
}
