package tap

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-openapi/spec"
	"github.com/julienschmidt/httprouter"
	pb "github.com/linkerd/linkerd2/controller/gen/controller/tap"
	"github.com/linkerd/linkerd2/controller/gen/public"
	"github.com/linkerd/linkerd2/controller/k8s"
	pkgK8s "github.com/linkerd/linkerd2/pkg/k8s"
	"github.com/linkerd/linkerd2/pkg/protohttp"
	"github.com/linkerd/linkerd2/pkg/tap"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
)

type handler struct {
	k8sAPI         *k8s.API
	usernameHeader string
	groupHeader    string
	tapClient      pb.TapClient
	log            *logrus.Entry
}

// TODO: share with api_handlers.go
type jsonError struct {
	Error string `json:"error"`
}

var (
	gvk = &schema.GroupVersionKind{
		Group:   "tap.linkerd.io",
		Version: "v1alpha1",
		Kind:    "Tap",
	}

	gvfd = metav1.GroupVersionForDiscovery{
		GroupVersion: gvk.GroupVersion().String(),
		Version:      gvk.Version,
	}

	apiGroup = metav1.APIGroup{
		Name:             gvk.Group,
		Versions:         []metav1.GroupVersionForDiscovery{gvfd},
		PreferredVersion: gvfd,
	}

	resources = []struct {
		name       string
		shortname  string
		namespaced bool
	}{
		{"namespaces", "ns", false},
		{"pods", "po", true},
		{"replicationcontrollers", "rc", true},
		{"services", "svc", true},
		{"daemonsets", "ds", true},
		{"deployments", "deploy", true},
		{"replicasets", "rs", true},
		{"statefulsets", "sts", true},
		{"jobs", "", true},
	}
)

func initRouter(h *handler) *httprouter.Router {
	router := &httprouter.Router{}

	router.GET("/", handleRoot)
	router.GET("/apis", handleAPIs)
	router.GET("/apis/"+gvk.Group, handleAPIGroup)
	router.GET("/apis/"+gvk.GroupVersion().String(), handleAPIResourceList)
	router.GET("/healthz", handleHealthz)
	router.GET("/healthz/log", handleHealthz)
	router.GET("/healthz/ping", handleHealthz)
	router.GET("/metrics", handleMetrics)
	router.GET("/openapi/v2", handleOpenAPI)
	router.GET("/version", handleVersion)
	router.NotFound = handleNotFound

	for _, res := range resources {
		route := ""
		if !res.namespaced {
			route = fmt.Sprintf("/apis/%s/watch/%s/:namespace", gvk.GroupVersion().String(), res.name)
		} else {
			route = fmt.Sprintf("/apis/%s/watch/namespaces/:namespace/%s/:name", gvk.GroupVersion().String(), res.name)
		}

		router.GET(route, handleRoot)
		router.POST(route+"/tap", h.handleTap)
	}

	return router
}

// POST /apis/tap.linkerd.io/v1alpha1/watch/namespaces/:namespace/tap
// POST /apis/tap.linkerd.io/v1alpha1/watch/namespaces/:namespace/:resource/:name/tap
func (h *handler) handleTap(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
	namespace := p.ByName("namespace")
	name := p.ByName("name")
	resource := ""

	path := strings.Split(req.URL.Path, "/")
	if len(path) == 8 {
		resource = path[5]
	} else if len(path) == 10 {
		resource = path[7]
	} else {
		err := fmt.Errorf("invalid path: %s", req.URL.Path)
		h.log.Error(err)
		renderJSONError(w, err, http.StatusBadRequest)
		return
	}

	h.log.Debugf("SubjectAccessReview: namespace: %s, resource: %s, name: %s, user: %s, group: %s",
		namespace, resource, name, req.Header.Get(h.usernameHeader), req.Header[h.groupHeader],
	)

	// TODO: it's possible this SubjectAccessReview is redundant, consider
	// removing, more info at https://github.com/linkerd/linkerd2/issues/3182
	err := pkgK8s.ResourceAuthzForUser(
		h.k8sAPI.Client,
		namespace,
		"watch",
		gvk.Group,
		gvk.Version,
		resource,
		"tap",
		name,
		req.Header.Get(h.usernameHeader),
		req.Header[h.groupHeader],
	)
	if err != nil {
		err = fmt.Errorf("tap authorization failed (%s), visit %s for more information", err, tap.TapRbacURL)
		h.log.Error(err)
		renderJSONError(w, err, http.StatusForbidden)
		return
	}

	tapReq := public.TapByResourceRequest{}
	err = protohttp.HTTPRequestToProto(req, &tapReq)
	if err != nil {
		err = fmt.Errorf("Error decoding Tap Request proto: %s", err)
		h.log.Error(err)
		protohttp.WriteErrorToHTTPResponse(w, err)
		return
	}

	url := protohttp.TapReqToURL(&tapReq)
	if url != req.URL.Path {
		err = fmt.Errorf("tap request body did not match APIServer URL: %+v != %+v", url, req.URL.Path)
		h.log.Error(err)
		protohttp.WriteErrorToHTTPResponse(w, err)
		return
	}

	flushableWriter, err := protohttp.NewStreamingWriter(w)
	if err != nil {
		h.log.Error(err)
		protohttp.WriteErrorToHTTPResponse(w, err)
		return
	}

	client, err := h.tapClient.TapByResource(req.Context(), &tapReq)
	if err != nil {
		h.log.Error(err)
		protohttp.WriteErrorToHTTPResponse(flushableWriter, err)
		return
	}

	for {
		select {
		case <-req.Context().Done():
			h.log.Debug("Received Done context in Tap Stream")
			return
		default:
			event, err := client.Recv()
			if err != nil {
				h.log.Errorf("Error receiving from tap client: %s", err)
				protohttp.WriteErrorToHTTPResponse(flushableWriter, err)
				return
			}
			err = protohttp.WriteProtoToHTTPResponse(flushableWriter, event)
			if err != nil {
				h.log.Errorf("Error writing proto to HTTP Response: %s", err)
				protohttp.WriteErrorToHTTPResponse(flushableWriter, err)
				return
			}
			flushableWriter.Flush()
		}
	}
}

// GET (not found)
func handleNotFound(w http.ResponseWriter, _ *http.Request) {
	handlePaths(w, http.StatusNotFound)
}

// GET /
// GET /apis/tap.linkerd.io/v1alpha1/watch/namespaces/:namespace
// GET /apis/tap.linkerd.io/v1alpha1/watch/namespaces/:namespace/:resource/:name
func handleRoot(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	handlePaths(w, http.StatusOK)
}

// GET /
// GET (not found)
func handlePaths(w http.ResponseWriter, status int) {
	paths := map[string][]string{
		"paths": {
			"/apis",
			"/apis/" + gvk.Group,
			"/apis/" + gvk.GroupVersion().String(),
			"/healthz",
			"/healthz/log",
			"/healthz/ping",
			"/metrics",
			"/openapi/v2",
			"/version",
		},
	}

	renderJSON(w, paths, status)
}

// GET /apis
func handleAPIs(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	groupList := metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{
			Kind: "APIGroupList",
		},
		Groups: []metav1.APIGroup{
			apiGroup,
		},
	}

	renderJSON(w, groupList, http.StatusOK)
}

// GET /apis/tap.linkerd.io
func handleAPIGroup(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	groupWithType := apiGroup
	groupWithType.TypeMeta = metav1.TypeMeta{
		Kind:       "APIGroup",
		APIVersion: "v1",
	}

	renderJSON(w, groupWithType, http.StatusOK)
}

// GET /apis/tap.linkerd.io/v1alpha1
// this is required for `kubectl api-resources` to work
func handleAPIResourceList(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	resList := metav1.APIResourceList{
		TypeMeta: metav1.TypeMeta{
			Kind:       "APIResourceList",
			APIVersion: "v1",
		},
		GroupVersion: gvk.GroupVersion().String(),
		APIResources: []metav1.APIResource{},
	}

	for _, res := range resources {
		resList.APIResources = append(resList.APIResources,
			metav1.APIResource{
				Name:       res.name,
				ShortNames: []string{res.shortname},
				Namespaced: res.namespaced,
				Kind:       gvk.Kind,
				Verbs:      metav1.Verbs{"watch"},
			})
		resList.APIResources = append(resList.APIResources,
			metav1.APIResource{
				Name:       fmt.Sprintf("%s/tap", res.name),
				Namespaced: res.namespaced,
				Kind:       gvk.Kind,
				Verbs:      metav1.Verbs{"watch"},
			})
	}

	renderJSON(w, resList, http.StatusOK)
}

// GET /healthz
// GET /healthz/logs
// GET /healthz/ping
func handleHealthz(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

// GET /metrics
func handleMetrics(w http.ResponseWriter, req *http.Request, _ httprouter.Params) {
	promhttp.Handler().ServeHTTP(w, req)
}

// GET /openapi/v2
func handleOpenAPI(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	swagger := spec.Swagger{
		SwaggerProps: spec.SwaggerProps{
			Swagger: "2.0",
			Info: &spec.Info{
				InfoProps: spec.InfoProps{
					Title:   "Api",
					Version: "v0",
				},
			},
			Paths: &spec.Paths{
				Paths: map[string]spec.PathItem{
					"/":                                    mkPathItem("get all paths"),
					"/apis":                                mkPathItem("get available API versions"),
					"/apis/" + gvk.Group:                   mkPathItem("get information of a group"),
					"/apis/" + gvk.GroupVersion().String(): mkPathItem("get available resources"),
				},
			},
		},
	}

	renderJSON(w, swagger, http.StatusOK)
}

func mkPathItem(desc string) spec.PathItem {
	return spec.PathItem{
		PathItemProps: spec.PathItemProps{
			Get: &spec.Operation{
				OperationProps: spec.OperationProps{
					Description: desc,
					Consumes:    []string{"application/json"},
					Produces:    []string{"application/json"},
				},
			},
		},
	}
}

// GET /version
func handleVersion(w http.ResponseWriter, _ *http.Request, _ httprouter.Params) {
	renderJSON(w, version.Info{}, http.StatusOK)
}

func renderJSON(w http.ResponseWriter, obj interface{}, status int) {
	bytes, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		renderJSONError(w, err, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(bytes)
}

// TODO: share with api_handlers.go
func renderJSONError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	rsp, _ := json.Marshal(jsonError{Error: err.Error()})
	w.WriteHeader(status)
	w.Write(rsp)
}
