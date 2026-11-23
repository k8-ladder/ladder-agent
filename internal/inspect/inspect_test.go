package inspect

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func mapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Pod"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Secret"}, meta.RESTScopeNamespace)
	m.Add(schema.GroupVersionKind{Version: "v1", Kind: "Node"}, meta.RESTScopeRoot)
	m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, meta.RESTScopeNamespace)
	return m
}

type rawCall struct {
	path, accept string
	query        url.Values
}

func reader(objs []runtime.Object, raw map[string]string) (*Reader, *[]rawCall) {
	var calls []rawCall
	r := &Reader{
		Kube:   fake.NewClientset(objs...),
		Mapper: mapper(),
		Raw: func(_ context.Context, p string, q url.Values, accept string) ([]byte, error) {
			calls = append(calls, rawCall{p, accept, q})
			body, ok := raw[p]
			if !ok {
				return nil, &notFound{p}
			}
			return []byte(body), nil
		},
		Now: func() time.Time { return now },
	}
	return r, &calls
}

type notFound struct{ path string }

func (e *notFound) Error() string { return e.path + " not found" }

func table(t *testing.T, cols []metav1.TableColumnDefinition, rows ...metav1.TableRow) string {
	t.Helper()
	b, err := json.Marshal(metav1.Table{ColumnDefinitions: cols, Rows: rows})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func row(ns string, cells ...any) metav1.TableRow {
	meta, _ := json.Marshal(metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: ns}})
	return metav1.TableRow{Cells: cells, Object: runtime.RawExtension{Raw: meta}}
}

func TestListPrintsTheServerColumns(t *testing.T) {
	cols := []metav1.TableColumnDefinition{
		{Name: "Name"}, {Name: "Ready"}, {Name: "Status"}, {Name: "Restarts", Type: "integer"}, {Name: "Age"}, {Name: "IP", Priority: 1},
	}
	r, calls := reader(nil, map[string]string{
		"/api/v1/namespaces/shop/pods": table(t, cols,
			row("shop", "web-7d9f", "0/1", "CrashLoopBackOff", 7, "5m", "10.0.0.1"),
			row("shop", "web-8a1c", "1/1", "Running", 0, "2d", "10.0.0.2")),
	})
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "list", Kind: "pods", Namespace: "shop", LabelSelector: "app=web"})
	if err != nil {
		t.Fatal(err)
	}
	want := "NAME       READY   STATUS             RESTARTS   AGE\n" +
		"web-7d9f   0/1     CrashLoopBackOff   7          5m\n" +
		"web-8a1c   1/1     Running            0          2d"
	if res.Text != want {
		t.Errorf("text:\n%s\nwant:\n%s", res.Text, want)
	}
	c := (*calls)[0]
	if !strings.Contains(c.accept, "as=Table") || c.query.Get("labelSelector") != "app=web" {
		t.Errorf("call = %+v", c)
	}
}

func TestListAllNamespacesAddsTheNamespace(t *testing.T) {
	cols := []metav1.TableColumnDefinition{{Name: "Name"}, {Name: "Ready"}}
	tbl := metav1.Table{ColumnDefinitions: cols, Rows: []metav1.TableRow{row("shop", "web", "1/1"), row("blog", "api", "0/2")}}
	tbl.Continue = "next"
	b, _ := json.Marshal(tbl)
	r, _ := reader(nil, map[string]string{"/apis/apps/v1/deployments": string(b)})
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "list", Kind: "Deployment"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "NAMESPACE   NAME   READY\nshop        web    1/1\nblog        api    0/2\n") {
		t.Errorf("text:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "only the first 200") {
		t.Errorf("no note on more rows:\n%s", res.Text)
	}
}

func TestListEmpty(t *testing.T) {
	r, _ := reader(nil, map[string]string{"/api/v1/nodes": table(t, []metav1.TableColumnDefinition{{Name: "Name"}})})
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "list", Kind: "node", Namespace: "ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "No nodes found in the cluster." {
		t.Errorf("text = %q", res.Text)
	}
}

func TestGetHidesSecretValues(t *testing.T) {
	secret := `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"shop",
		"managedFields":[{"manager":"kubectl"}],
		"annotations":{"kubectl.kubernetes.io/last-applied-configuration":"{\"data\":{\"password\":\"aHVudGVyMg==\"}}"}},
		"type":"Opaque","data":{"password":"aHVudGVyMg==","user":"YQ=="},"stringData":{"token":"abc"}}`
	r, _ := reader(nil, map[string]string{"/api/v1/namespaces/shop/secrets/db": secret})
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "get", Kind: "Secret", Namespace: "shop", Name: "db"})
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"aHVudGVyMg", "YQ==", "abc", "last-applied", "managedFields"} {
		if strings.Contains(res.Text, leak) {
			t.Errorf("text has %q:\n%s", leak, res.Text)
		}
	}
	for _, want := range []string{"password: <hidden, 7 bytes>", "user: <hidden, 1 bytes>", "token: <hidden, 3 bytes>", "type: Opaque"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text has no %q:\n%s", want, res.Text)
		}
	}
}

func TestGetKeepsOtherObjects(t *testing.T) {
	dep := `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"shop","annotations":{"team":"a"}},
		"spec":{"replicas":2},"data":{"x":"z"}}`
	r, _ := reader(nil, map[string]string{"/apis/apps/v1/namespaces/shop/deployments/web": dep})
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "get", Kind: "deployments.apps", Namespace: "shop", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"replicas: 2", "team: a", "x: z"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("text has no %q:\n%s", want, res.Text)
		}
	}
}

func TestBadRequests(t *testing.T) {
	r, _ := reader(nil, nil)
	for _, req := range []protocol.KubeReadRequest{
		{Op: "delete", Kind: "pods"},
		{Op: "list"},
		{Op: "list", Kind: "widgets"},
		{Op: "get", Kind: "pods", Namespace: "shop"},
		{Op: "get", Kind: "pods", Name: "web"},
		{Op: "get", Kind: "pods", Namespace: "shop", Name: "web"},
		{Op: "logs", Namespace: "shop"},
		{Op: "logs", Kind: "Deployment", Namespace: "shop", Name: "web"},
	} {
		_, err := r.Read(context.Background(), req)
		var perr *protocol.Error
		if err == nil || !asProtocol(err, &perr) || perr.Code != protocol.CodeBadRequest {
			t.Errorf("%+v: err = %v, want bad_request", req, err)
		}
	}
}

// A name or namespace that is not one never reaches an API path: it could
// read a Secret through another kind, with its values, or the kubelet.
func TestNamesThatLeaveThePathAreRefused(t *testing.T) {
	r, calls := reader(nil, nil)
	for _, req := range []protocol.KubeReadRequest{
		{Op: "get", Kind: "pods", Namespace: "argocd", Name: "../secrets/argocd-initial-admin-secret"},
		{Op: "get", Kind: "nodes", Name: "node-1/proxy/logs/"},
		{Op: "get", Kind: "nodes", Name: ".."},
		{Op: "get", Kind: "pods", Namespace: "argocd/secrets", Name: "x"},
		{Op: "get", Kind: "pods", Namespace: "kube-system/../argocd", Name: "x"},
		{Op: "get", Kind: "pods", Namespace: "shop", Name: "web%2F..%2Fx"},
		{Op: "list", Kind: "pods", Namespace: "../.."},
		{Op: "logs", Namespace: "shop", Name: "web/../../x"},
		{Op: "logs", Namespace: "shop", Name: "web", Container: "app/x"},
		{Op: "events", Namespace: "shop", Name: "a/b"},
	} {
		_, err := r.Read(context.Background(), req)
		var perr *protocol.Error
		if err == nil || !asProtocol(err, &perr) || perr.Code != protocol.CodeBadRequest {
			t.Errorf("%+v: err = %v, want bad_request", req, err)
		}
	}
	if len(*calls) > 0 {
		t.Errorf("API calls: %+v, want none", *calls)
	}
}

// Names Kubernetes allows still read, such as a ClusterRole's with colons.
func TestNamesWithColonsRead(t *testing.T) {
	if err := CheckNames(protocol.KubeReadRequest{Name: "system:aggregate-to-view", Namespace: "kube-system"}); err != nil {
		t.Fatal(err)
	}
}

func asProtocol(err error, perr **protocol.Error) bool {
	p, ok := err.(*protocol.Error)
	*perr = p
	return ok
}

func TestEvents(t *testing.T) {
	ev := func(name, reason, msg string, ago time.Duration, count int32) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "shop"},
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "web-7d9f", Namespace: "shop"},
			Type:           "Warning", Reason: reason, Message: msg, Count: count,
			LastTimestamp: metav1.NewTime(now.Add(-ago)),
		}
	}
	r, _ := reader([]runtime.Object{
		ev("b", "BackOff", "Back-off restarting failed container web", time.Minute, 12),
		ev("a", "Failed", "Failed to pull image \"web:1.4.3\":\n  not found", 10*time.Minute, 1),
	}, nil)
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "events", Namespace: "shop", Kind: "pod", Name: "web-7d9f"})
	if err != nil {
		t.Fatal(err)
	}
	want := "LAST SEEN   TYPE      REASON    OBJECT         MESSAGE\n" +
		"10m         Warning   Failed    pod/web-7d9f   Failed to pull image \"web:1.4.3\": not found\n" +
		"60s (x12)   Warning   BackOff   pod/web-7d9f   Back-off restarting failed container web"
	if res.Text != want {
		t.Errorf("text:\n%s\nwant:\n%s", res.Text, want)
	}
}

func TestEventsKeepTheNewest(t *testing.T) {
	var events []corev1.Event
	for i := range maxEvents + 5 {
		events = append(events, corev1.Event{Reason: "R", LastTimestamp: metav1.NewTime(now.Add(-time.Duration(i) * time.Minute))})
	}
	text := formatEvents(events, false, now)
	if !strings.HasPrefix(text, "… 5 older events not shown\n") {
		t.Errorf("text starts with %q", text[:40])
	}
	if strings.Count(text, "\n") != maxEvents+1 {
		t.Errorf("%d lines", strings.Count(text, "\n"))
	}
}

func TestLogs(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-7d9f", Namespace: "shop"},
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{{Name: "migrate"}},
			Containers:     []corev1.Container{{Name: "web"}, {Name: "proxy"}},
		},
	}
	r, _ := reader([]runtime.Object{pod}, nil)
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "logs", Kind: "Pod", Namespace: "shop", Name: "web-7d9f", Previous: true, TailLines: 5000})
	if err != nil {
		t.Fatal(err)
	}
	want := "Logs of pod web-7d9f, container web, previous run, last 1000 lines; containers: web, proxy; init containers: migrate:\nfake logs"
	if res.Text != want {
		t.Errorf("text = %q", res.Text)
	}
	if _, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "logs", Namespace: "shop", Name: "web-7d9f", Container: "db"}); err == nil ||
		!strings.Contains(err.Error(), "containers: web, proxy; init containers: migrate") {
		t.Errorf("err = %v", err)
	}
	if _, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "logs", Namespace: "shop", Name: "gone"}); err == nil ||
		!strings.Contains(err.Error(), `"gone" not found`) {
		t.Errorf("err = %v", err)
	}
}

func TestTruncate(t *testing.T) {
	r, _ := reader(nil, map[string]string{"/apis/apps/v1/namespaces/shop/deployments/web": `{"metadata":{"name":"web"},"spec":{"x":"` + strings.Repeat("a", 2*MaxText) + `"}}`})
	res, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "get", Kind: "deployment", Namespace: "shop", Name: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Text) > MaxText+64 || !strings.HasSuffix(res.Text, "(cut at 32 KiB)") {
		t.Errorf("truncated = %v, %d bytes", res.Truncated, len(res.Text))
	}
}

// The people who run the cluster turn reads off, logs off, or limit them to
// some namespaces; the cloud cannot change that.
func TestReadLimits(t *testing.T) {
	r, calls := reader(nil, nil)
	r.Off = true
	if _, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "list", Kind: "pods", Namespace: "shop"}); err == nil {
		t.Error("reads turned off, yet read")
	}
	r.Off, r.NoLogs = false, true
	if _, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "logs", Namespace: "shop", Name: "web"}); err == nil {
		t.Error("logs turned off, yet read")
	}
	r.NoLogs, r.Namespaces = false, []string{"shop-*"}
	for _, req := range []protocol.KubeReadRequest{
		{Op: "list", Kind: "pods"},
		{Op: "list", Kind: "pods", Namespace: "kube-system"},
		{Op: "get", Kind: "pods", Namespace: "argocd", Name: "x"},
		{Op: "events"},
		{Op: "events", Namespace: "billing"},
		{Op: "logs", Namespace: "billing", Name: "web"},
	} {
		var perr *protocol.Error
		if _, err := r.Read(context.Background(), req); !asProtocol(err, &perr) || perr.Code != protocol.CodeFailedPrecondition {
			t.Errorf("%+v outside the namespaces: %v", req, err)
		}
	}
	if len(*calls) > 0 {
		t.Errorf("API calls: %+v", *calls)
	}
	// Inside them, and cluster-wide kinds, read.
	if _, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "list", Kind: "nodes"}); err != nil && strings.Contains(err.Error(), "LADDER_READ_NAMESPACES") {
		t.Errorf("nodes: %v", err)
	}
	if _, err := r.Read(context.Background(), protocol.KubeReadRequest{Op: "list", Kind: "pods", Namespace: "shop-prod"}); err != nil && strings.Contains(err.Error(), "LADDER_READ_NAMESPACES") {
		t.Errorf("shop-prod: %v", err)
	}
}
