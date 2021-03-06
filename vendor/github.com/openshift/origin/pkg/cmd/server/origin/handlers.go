package origin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"

	restful "github.com/emicklei/go-restful"
	"github.com/golang/glog"

	kapi "k8s.io/kubernetes/pkg/api"
	kapierrors "k8s.io/kubernetes/pkg/api/errors"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apiserver"
	"k8s.io/kubernetes/pkg/auth/user"
	"k8s.io/kubernetes/pkg/httplog"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/serviceaccount"
	"k8s.io/kubernetes/pkg/util/sets"

	authorizationapi "github.com/openshift/origin/pkg/authorization/api"
	"github.com/openshift/origin/pkg/authorization/authorizer"
	configapi "github.com/openshift/origin/pkg/cmd/server/api"
	"github.com/openshift/origin/pkg/cmd/server/bootstrappolicy"
	userapi "github.com/openshift/origin/pkg/user/api"
	uservalidation "github.com/openshift/origin/pkg/user/api/validation"
	"github.com/openshift/origin/pkg/util/httprequest"
)

// TODO We would like to use the IndexHandler from k8s but we do not yet have a
// MuxHelper to track all registered paths
func indexAPIPaths(osAPIVersions, kubeAPIVersions []string, handler http.Handler) http.Handler {
	// TODO once we have a MuxHelper we will not need to hardcode this list of paths
	rootPaths := []string{"/api",
		"/apis",
		"/controllers",
		"/healthz",
		"/healthz/ping",
		"/healthz/ready",
		"/metrics",
		"/oapi",
		"/swaggerapi/"}

	// This is for legacy clients
	// Discovery of new API groups is done with a request to /apis
	for _, path := range kubeAPIVersions {
		rootPaths = append(rootPaths, "/api/"+path)
	}
	for _, path := range osAPIVersions {
		rootPaths = append(rootPaths, "/oapi/"+path)
	}
	sort.Strings(rootPaths)

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			output, err := json.MarshalIndent(unversioned.RootPaths{Paths: rootPaths}, "", "  ")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", restful.MIME_JSON)
			w.WriteHeader(http.StatusOK)
			w.Write(output)
		} else {
			handler.ServeHTTP(w, req)
		}
	})
}

func (c *MasterConfig) authorizationFilter(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		attributes, err := c.AuthorizationAttributeBuilder.GetAttributes(req)
		if err != nil {
			forbidden(err.Error(), attributes, w, req)
			return
		}
		if attributes == nil {
			forbidden("No attributes", attributes, w, req)
			return
		}

		ctx, exists := c.RequestContextMapper.Get(req)
		if !exists {
			forbidden("context not found", attributes, w, req)
			return
		}

		allowed, reason, err := c.Authorizer.Authorize(ctx, attributes)
		if err != nil {
			forbidden(err.Error(), attributes, w, req)
			return
		}
		if !allowed {
			forbidden(reason, attributes, w, req)
			return
		}

		handler.ServeHTTP(w, req)
	})
}

// forbidden renders a simple forbidden error
func forbidden(reason string, attributes authorizer.AuthorizationAttributes, w http.ResponseWriter, req *http.Request) {
	kind := ""
	resource := ""
	group := ""
	name := ""
	// the attributes can be empty for two basic reasons:
	// 1. malformed API request
	// 2. not an API request at all
	// In these cases, just assume default that will work better than nothing
	if attributes != nil {
		group = attributes.GetAPIGroup()
		resource = attributes.GetResource()
		kind = attributes.GetResource()
		if len(attributes.GetAPIGroup()) > 0 {
			kind = attributes.GetAPIGroup() + "." + kind
		}
		name = attributes.GetResourceName()
	}

	// Reason is an opaque string that describes why access is allowed or forbidden (forbidden by the time we reach here).
	// We don't have direct access to kind or name (not that those apply either in the general case)
	// We create a NewForbidden to stay close the API, but then we override the message to get a serialization
	// that makes sense when a human reads it.
	forbiddenError, _ := kapierrors.NewForbidden(unversioned.GroupResource{Group: group, Resource: resource}, name, errors.New("") /*discarded*/).(*kapierrors.StatusError)
	forbiddenError.ErrStatus.Message = reason

	formatted := &bytes.Buffer{}
	output, err := runtime.Encode(kapi.Codecs.LegacyCodec(kapi.SchemeGroupVersion), &forbiddenError.ErrStatus)
	if err != nil {
		fmt.Fprintf(formatted, "%s", forbiddenError.Error())
	} else {
		json.Indent(formatted, output, "", "  ")
	}

	w.Header().Set("Content-Type", restful.MIME_JSON)
	w.WriteHeader(http.StatusForbidden)
	w.Write(formatted.Bytes())
}

// cacheControlFilter sets the Cache-Control header to the specified value.
func cacheControlFilter(handler http.Handler, value string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", value)
		handler.ServeHTTP(w, req)
	})
}

// namespacingFilter adds a filter that adds the namespace of the request to the context.  Not all requests will have namespaces,
// but any that do will have the appropriate value added.
func namespacingFilter(handler http.Handler, contextMapper kapi.RequestContextMapper) http.Handler {
	infoResolver := &apiserver.RequestInfoResolver{APIPrefixes: sets.NewString("api", "osapi", "oapi", "apis"), GrouplessAPIPrefixes: sets.NewString("api", "osapi", "oapi")}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ctx, ok := contextMapper.Get(req)
		if !ok {
			http.Error(w, "Unable to find request context", http.StatusInternalServerError)
			return
		}

		if _, exists := kapi.NamespaceFrom(ctx); !exists {
			if requestInfo, err := infoResolver.GetRequestInfo(req); err == nil {
				// only set the namespace if the apiRequestInfo was resolved
				// keep in mind that GetAPIRequestInfo will fail on non-api requests, so don't fail the entire http request on that
				// kind of failure.

				// TODO reconsider special casing this.  Having the special case hereallow us to fully share the kube
				// APIRequestInfoResolver without any modification or customization.
				namespace := requestInfo.Namespace
				if (requestInfo.Resource == "projects") && (len(requestInfo.Name) > 0) {
					namespace = requestInfo.Name
				}

				ctx = kapi.WithNamespace(ctx, namespace)
				contextMapper.Update(req, ctx)
			}
		}

		handler.ServeHTTP(w, req)
	})
}

type userAgentFilter struct {
	regex   *regexp.Regexp
	message string
	verbs   sets.String
}

func newUserAgentFilter(config configapi.UserAgentMatchRule) (userAgentFilter, error) {
	regex, err := regexp.Compile(config.Regex)
	if err != nil {
		return userAgentFilter{}, err
	}
	userAgentFilter := userAgentFilter{regex: regex, verbs: sets.NewString(config.HTTPVerbs...)}

	return userAgentFilter, nil
}

func (f *userAgentFilter) matches(verb, userAgent string) bool {
	if len(f.verbs) > 0 && !f.verbs.Has(verb) {
		return false
	}

	return f.regex.MatchString(userAgent)
}

// versionSkewFilter adds a filter that may deny requests from skewed
// oc clients, since we know that those clients will strip unknown fields which can lead to unexpected outcomes
func (c *MasterConfig) versionSkewFilter(handler http.Handler) http.Handler {
	infoResolver := &apiserver.RequestInfoResolver{APIPrefixes: sets.NewString("api", "osapi", "oapi", "apis"), GrouplessAPIPrefixes: sets.NewString("api", "osapi", "oapi")}

	filterConfig := c.Options.PolicyConfig.UserAgentMatchingConfig
	if len(filterConfig.RequiredClients) == 0 && len(filterConfig.DeniedClients) == 0 {
		return handler
	}

	defaultMessage := filterConfig.DefaultRejectionMessage
	if len(defaultMessage) == 0 {
		defaultMessage = "the cluster administrator has disabled access for this client, please upgrade or consult your administrator"
	}

	// the structure of the legacyClientPolicyConfig is pretty easy to write, but its inefficient to use at runtime
	// pre-process the config elements to make a more efficicent structure.
	allowedFilters := []userAgentFilter{}
	deniedFilters := []userAgentFilter{}
	for _, config := range filterConfig.RequiredClients {
		userAgentFilter, err := newUserAgentFilter(config)
		if err != nil {
			glog.Errorf("Failure to compile User-Agent regex %v: %v", config.Regex, err)
			continue
		}

		allowedFilters = append(allowedFilters, userAgentFilter)
	}
	for _, config := range filterConfig.DeniedClients {
		userAgentFilter, err := newUserAgentFilter(config.UserAgentMatchRule)
		if err != nil {
			glog.Errorf("Failure to compile User-Agent regex %v: %v", config.Regex, err)
			continue
		}
		userAgentFilter.message = config.RejectionMessage
		if len(userAgentFilter.message) == 0 {
			userAgentFilter.message = defaultMessage
		}

		deniedFilters = append(deniedFilters, userAgentFilter)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if requestInfo, err := infoResolver.GetRequestInfo(req); err == nil && !requestInfo.IsResourceRequest {
			handler.ServeHTTP(w, req)
			return
		}

		userAgent := req.Header.Get("User-Agent")

		if len(allowedFilters) > 0 {
			foundMatch := false
			for _, filter := range allowedFilters {
				if filter.matches(req.Method, userAgent) {
					foundMatch = true
					break
				}
			}

			if !foundMatch {
				forbidden(defaultMessage, nil, w, req)
				return
			}
		}

		for _, filter := range deniedFilters {
			if filter.matches(req.Method, userAgent) {
				forbidden(filter.message, nil, w, req)
				return
			}
		}

		handler.ServeHTTP(w, req)
	})
}

// If we know the location of the asset server, redirect to it when / is requested
// and the Accept header supports text/html
func assetServerRedirect(handler http.Handler, assetPublicURL string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/" {
			if httprequest.PrefersHTML(req) {
				http.Redirect(w, req, assetPublicURL, http.StatusFound)
			}
		}
		// Dispatch to the next handler
		handler.ServeHTTP(w, req)
	})
}

func (c *MasterConfig) impersonationFilter(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestedSubject := req.Header.Get("Impersonate-User")
		if len(requestedSubject) == 0 {
			handler.ServeHTTP(w, req)
			return
		}

		resource, namespace, name, err := parseRequestedSubject(requestedSubject)
		if err != nil {
			forbidden(err.Error(), nil, w, req)
			return
		}

		ctx, exists := c.RequestContextMapper.Get(req)
		if !exists {
			forbidden("context not found", nil, w, req)
			return
		}

		actingAsAttributes := &authorizer.DefaultAuthorizationAttributes{
			Verb:         "impersonate",
			APIGroup:     resource.Group,
			Resource:     resource.Resource,
			ResourceName: name,
		}
		authCheckCtx := kapi.WithNamespace(ctx, namespace)

		allowed, reason, err := c.Authorizer.Authorize(authCheckCtx, actingAsAttributes)
		if err != nil {
			forbidden(err.Error(), actingAsAttributes, w, req)
			return
		}
		if !allowed {
			forbidden(reason, actingAsAttributes, w, req)
			return
		}

		switch resource {
		case kapi.Resource(authorizationapi.ServiceAccountResource):
			newUser := &user.DefaultInfo{
				Name:   serviceaccount.MakeUsername(namespace, name),
				Groups: serviceaccount.MakeGroupNames(namespace, name),
			}
			newUser.Groups = append(newUser.Groups, bootstrappolicy.AuthenticatedGroup)
			c.RequestContextMapper.Update(req, kapi.WithUser(ctx, newUser))

		case userapi.Resource(authorizationapi.UserResource):
			newUser := &user.DefaultInfo{
				Name: name,
			}
			groups, err := c.GroupCache.GroupsFor(name)
			if err == nil {
				for _, group := range groups {
					newUser.Groups = append(newUser.Groups, group.Name)
				}
			}

			newUser.Groups = append(newUser.Groups, bootstrappolicy.AuthenticatedGroup, bootstrappolicy.AuthenticatedOAuthGroup)
			c.RequestContextMapper.Update(req, kapi.WithUser(ctx, newUser))

		case userapi.Resource(authorizationapi.SystemUserResource):
			newUser := &user.DefaultInfo{
				Name: name,
			}

			if name == bootstrappolicy.UnauthenticatedUsername {
				newUser.Groups = append(newUser.Groups, bootstrappolicy.UnauthenticatedGroup)
			} else {
				newUser.Groups = append(newUser.Groups, bootstrappolicy.AuthenticatedGroup)
			}
			c.RequestContextMapper.Update(req, kapi.WithUser(ctx, newUser))

		default:
			forbidden(fmt.Sprintf("%v is an unhandled resource for acting-as", resource), nil, w, req)
			return
		}

		newCtx, _ := c.RequestContextMapper.Get(req)
		oldUser, _ := kapi.UserFrom(ctx)
		newUser, _ := kapi.UserFrom(newCtx)
		httplog.LogOf(req, w).Addf("%v is acting as %v", oldUser, newUser)

		handler.ServeHTTP(w, req)
	})
}

func parseRequestedSubject(requestedSubject string) (unversioned.GroupResource, string, string, error) {
	subjects := authorizationapi.BuildSubjects([]string{requestedSubject}, nil,
		// validates whether the usernames are regular users or system users
		uservalidation.ValidateUserName,
		// validates group names, but we never pass any groups
		func(s string, b bool) (bool, string) { return true, "" })

	if len(subjects) == 0 {
		return unversioned.GroupResource{}, "", "", fmt.Errorf("subject must be in the form of a username, not %v", requestedSubject)

	}

	resource := unversioned.GroupResource{}
	switch subjects[0].GetObjectKind().GroupVersionKind().GroupKind() {
	case userapi.Kind(authorizationapi.UserKind):
		resource = userapi.Resource(authorizationapi.UserResource)

	case userapi.Kind(authorizationapi.SystemUserKind):
		resource = userapi.Resource(authorizationapi.SystemUserResource)

	case kapi.Kind(authorizationapi.ServiceAccountKind):
		resource = kapi.Resource(authorizationapi.ServiceAccountResource)

	default:
		return unversioned.GroupResource{}, "", "", fmt.Errorf("unknown subject type: %v", subjects[0])
	}

	return resource, subjects[0].Namespace, subjects[0].Name, nil
}
