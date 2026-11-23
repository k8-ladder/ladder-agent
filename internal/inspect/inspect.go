// Package inspect serves kube.read: read-only looks at the cluster for the
// cloud's assistant, as kubectl get, get -o yaml, events and logs print them.
// It never changes anything, and never returns the values of a Secret.
package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ladder/ladder-agent/internal/kube"
	"github.com/ladder/ladder-agent/internal/protocol"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/validation/path"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/yaml"
)

const (
	// MaxText is the most text one read returns.
	MaxText     = 32 << 10
	maxRows     = 200
	maxEvents   = 60
	defaultTail = 100
	maxTail     = 1000

	tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io,application/json"
)

// Reader answers kube.read.
type Reader struct {
	Kube kubernetes.Interface
	// Mapper resolves kinds, resources and short names.
	Mapper meta.RESTMapper
	// Reset forgets what the mapper read, for a kind the cluster serves
	// since then, such as a CRD installed later. May be nil.
	Reset func()
	// Raw GETs an API path, for tables and objects of any kind.
	Raw func(ctx context.Context, path string, query url.Values, accept string) ([]byte, error)
	Now func() time.Time
}

// New reads the cluster of k.
func New(k *kube.Clients) *Reader {
	rest := k.Kube.Discovery().RESTClient()
	return &Reader{
		Kube:   k.Kube,
		Mapper: restmapper.NewShortcutExpander(k.Mapper, k.Discovery, nil),
		Reset:  k.Mapper.Reset,
		Raw: func(ctx context.Context, p string, query url.Values, accept string) ([]byte, error) {
			req := rest.Get().AbsPath(p).SetHeader("Accept", accept)
			for key, vs := range query {
				for _, v := range vs {
					req = req.Param(key, v)
				}
			}
			return req.DoRaw(ctx)
		},
		Now: time.Now,
	}
}

// Read serves one kube.read request.
func (r *Reader) Read(ctx context.Context, req protocol.KubeReadRequest) (*protocol.KubeReadResult, error) {
	if err := CheckNames(req); err != nil {
		return nil, err
	}
	var (
		text string
		err  error
	)
	switch req.Op {
	case protocol.KubeList:
		text, err = r.list(ctx, req)
	case protocol.KubeGet:
		text, err = r.get(ctx, req)
	case protocol.KubeEvents:
		text, err = r.events(ctx, req)
	case protocol.KubeLogs:
		text, err = r.logs(ctx, req)
	default:
		return nil, protocol.Errorf(protocol.CodeBadRequest, "op must be list, get, events or logs")
	}
	if err != nil {
		var perr *protocol.Error
		if errors.As(err, &perr) || ctx.Err() != nil {
			return nil, err
		}
		// What the API server said, such as `pods "web" not found`.
		return nil, protocol.Errorf(protocol.CodeBadRequest, "%v", err)
	}
	res := &protocol.KubeReadResult{Text: text}
	if len(text) > MaxText {
		res.Truncated = true
		if req.Op == protocol.KubeLogs {
			// The newest lines matter most.
			res.Text = "… (earlier lines cut)\n" + text[len(text)-MaxText:]
		} else {
			res.Text = text[:MaxText] + "\n… (cut at 32 KiB)"
		}
	}
	return res, nil
}

// CheckNames refuses a namespace, name or container that is not one, before
// any of them goes into an API path. The path is built by hand for objects of
// any kind, and a name such as "../secrets/x" or "node-1/proxy/logs" would
// read another resource than the kind asked for: a Secret through "configmaps",
// with its values, or the kubelet through a node.
func CheckNames(req protocol.KubeReadRequest) error {
	if req.Namespace != "" {
		if errs := validation.IsDNS1123Label(req.Namespace); len(errs) > 0 {
			return protocol.Errorf(protocol.CodeBadRequest, "namespace %q is not a namespace name: %s", req.Namespace, strings.Join(errs, "; "))
		}
	}
	if req.Name != "" {
		if errs := path.IsValidPathSegmentName(req.Name); len(errs) > 0 {
			return protocol.Errorf(protocol.CodeBadRequest, "name %q is not an object name: %s", req.Name, strings.Join(errs, "; "))
		}
	}
	if req.Container != "" {
		if errs := validation.IsDNS1123Label(req.Container); len(errs) > 0 {
			return protocol.Errorf(protocol.CodeBadRequest, "container %q is not a container name: %s", req.Container, strings.Join(errs, "; "))
		}
	}
	return nil
}

// resource is a kind as the API serves it.
type resource struct {
	gvr        schema.GroupVersionResource
	kind       string
	namespaced bool
}

// path is the API path of the resource, in namespace ns if it is namespaced
// and ns is not empty, and of the object name if it is not empty.
func (res resource) path(ns, name string) string {
	p := "/apis/" + res.gvr.Group + "/" + res.gvr.Version
	if res.gvr.Group == "" {
		p = "/api/" + res.gvr.Version
	}
	if res.namespaced && ns != "" {
		p += "/namespaces/" + ns
	}
	p += "/" + res.gvr.Resource
	if name != "" {
		p += "/" + name
	}
	return p
}

func (res resource) secret() bool { return res.gvr.Group == "" && res.gvr.Resource == "secrets" }

// resolve maps a kind, resource or short name to the resource the API
// serves, reading discovery again once for a kind it does not know.
func (r *Reader) resolve(kind string) (resource, error) {
	if kind == "" {
		return resource{}, protocol.Errorf(protocol.CodeBadRequest, "kind is required")
	}
	res, err := r.lookup(kind)
	if meta.IsNoMatchError(err) && r.Reset != nil {
		r.Reset()
		res, err = r.lookup(kind)
	}
	if meta.IsNoMatchError(err) {
		return resource{}, protocol.Errorf(protocol.CodeBadRequest, "the cluster serves no kind %q", kind)
	}
	return res, err
}

func (r *Reader) lookup(kind string) (resource, error) {
	full, gr := schema.ParseResourceArg(strings.ToLower(kind))
	var (
		gvr schema.GroupVersionResource
		err error
	)
	if full != nil {
		gvr, err = r.Mapper.ResourceFor(*full)
	}
	if full == nil || err != nil {
		gvr, err = r.Mapper.ResourceFor(gr.WithVersion(""))
	}
	if err != nil {
		return resource{}, err
	}
	gvk, err := r.Mapper.KindFor(gvr)
	if err != nil {
		return resource{}, err
	}
	m, err := r.Mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return resource{}, err
	}
	return resource{gvr: m.Resource, kind: gvk.Kind, namespaced: m.Scope.Name() == meta.RESTScopeNameNamespace}, nil
}

// list prints objects as kubectl get does, with the columns the API server
// gives for the kind.
func (r *Reader) list(ctx context.Context, req protocol.KubeReadRequest) (string, error) {
	res, err := r.resolve(req.Kind)
	if err != nil {
		return "", err
	}
	q := url.Values{"limit": {fmt.Sprint(maxRows)}, "includeObject": {"Metadata"}}
	if req.LabelSelector != "" {
		q.Set("labelSelector", req.LabelSelector)
	}
	raw, err := r.Raw(ctx, res.path(req.Namespace, ""), q, tableAccept)
	if err != nil {
		return "", err
	}
	var t metav1.Table
	if err := json.Unmarshal(raw, &t); err != nil {
		return "", fmt.Errorf("read the table of %s: %w", res.gvr.Resource, err)
	}
	if len(t.Rows) == 0 {
		where := "in the cluster"
		if res.namespaced && req.Namespace != "" {
			where = "in namespace " + req.Namespace
		}
		return fmt.Sprintf("No %s found %s.", res.gvr.Resource, where), nil
	}
	text := formatTable(t, res.namespaced && req.Namespace == "")
	if t.Continue != "" {
		text += fmt.Sprintf("\n… only the first %d; narrow it with a namespace or a label selector.", maxRows)
	}
	return text, nil
}

// formatTable prints a Table as kubectl get does: the columns of priority 0,
// after the namespace when the rows come from all namespaces.
func formatTable(t metav1.Table, withNamespace bool) string {
	var cols []int
	var header []string
	if withNamespace {
		header = append(header, "NAMESPACE")
	}
	for i, c := range t.ColumnDefinitions {
		if c.Priority == 0 {
			cols = append(cols, i)
			header = append(header, strings.ToUpper(c.Name))
		}
	}
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, strings.Join(header, "\t"))
	for _, row := range t.Rows {
		var cells []string
		if withNamespace {
			var m metav1.PartialObjectMetadata
			_ = json.Unmarshal(row.Object.Raw, &m)
			cells = append(cells, m.Namespace)
		}
		for _, i := range cols {
			cell := ""
			if i < len(row.Cells) && row.Cells[i] != nil {
				cell = fmt.Sprint(row.Cells[i])
			}
			cells = append(cells, cell)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	w.Flush()
	return strings.TrimRight(b.String(), "\n")
}

// get prints one object as YAML, without managed fields and with the values
// of a Secret hidden.
func (r *Reader) get(ctx context.Context, req protocol.KubeReadRequest) (string, error) {
	if req.Name == "" {
		return "", protocol.Errorf(protocol.CodeBadRequest, "name is required")
	}
	res, err := r.resolve(req.Kind)
	if err != nil {
		return "", err
	}
	if res.namespaced && req.Namespace == "" {
		return "", protocol.Errorf(protocol.CodeBadRequest, "%s are namespaced: namespace is required", res.gvr.Resource)
	}
	raw, err := r.Raw(ctx, res.path(req.Namespace, req.Name), nil, "application/json")
	if err != nil {
		return "", err
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", fmt.Errorf("read %s %s: %w", res.kind, req.Name, err)
	}
	clean(obj, res.secret())
	out, err := yaml.Marshal(obj)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// clean drops what only adds noise, and hides what must not leave the
// cluster: the values of a Secret, and the copy of an object kubectl apply
// keeps in an annotation, which for a Secret holds its values too.
func clean(obj map[string]any, secret bool) {
	if md, ok := obj["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			delete(ann, corev1.LastAppliedConfigAnnotation)
			if len(ann) == 0 {
				delete(md, "annotations")
			}
		}
	}
	if !secret {
		return
	}
	for _, key := range []string{"data", "stringData"} {
		data, ok := obj[key].(map[string]any)
		if !ok {
			continue
		}
		for k, v := range data {
			s, _ := v.(string)
			n := len(s)
			if key == "data" {
				// base64: 4 characters for 3 bytes.
				n = len(s) * 3 / 4
				n -= strings.Count(s[max(0, len(s)-2):], "=")
			}
			data[k] = fmt.Sprintf("<hidden, %d bytes>", n)
		}
	}
}

// events prints events oldest first, as kubectl events does: in a namespace
// or all of them, of one object when kind and name are given.
func (r *Reader) events(ctx context.Context, req protocol.KubeReadRequest) (string, error) {
	sel := fields.Set{}
	if req.Name != "" {
		sel["involvedObject.name"] = req.Name
	}
	if req.Kind != "" {
		res, err := r.resolve(req.Kind)
		if err != nil {
			return "", err
		}
		sel["involvedObject.kind"] = res.kind
	}
	list, err := r.Kube.CoreV1().Events(req.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel.AsSelector().String()})
	if err != nil {
		return "", err
	}
	if len(list.Items) == 0 {
		return "No events found. Kubernetes keeps events for about an hour.", nil
	}
	return formatEvents(list.Items, req.Namespace == "", r.Now()), nil
}

func formatEvents(events []corev1.Event, withNamespace bool, now time.Time) string {
	slices.SortStableFunc(events, func(a, b corev1.Event) int { return lastSeen(a).Compare(lastSeen(b)) })
	skipped := 0
	if len(events) > maxEvents {
		skipped = len(events) - maxEvents
		events = events[skipped:]
	}
	var b bytes.Buffer
	w := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	header := "LAST SEEN\tTYPE\tREASON\tOBJECT\tMESSAGE"
	if withNamespace {
		header = "NAMESPACE\t" + header
	}
	fmt.Fprintln(w, header)
	for _, e := range events {
		seen := "<unknown>"
		if t := lastSeen(e); !t.IsZero() {
			seen = duration.HumanDuration(now.Sub(t))
		}
		count := e.Count
		if e.Series != nil {
			count = max(count, e.Series.Count)
		}
		if count > 1 {
			seen += fmt.Sprintf(" (x%d)", count)
		}
		obj := strings.ToLower(e.InvolvedObject.Kind) + "/" + e.InvolvedObject.Name
		line := strings.Join([]string{seen, e.Type, e.Reason, obj, strings.Join(strings.Fields(e.Message), " ")}, "\t")
		if withNamespace {
			line = e.Namespace + "\t" + line
		}
		fmt.Fprintln(w, line)
	}
	w.Flush()
	text := strings.TrimRight(b.String(), "\n")
	if skipped > 0 {
		text = fmt.Sprintf("… %d older events not shown\n", skipped) + text
	}
	return text
}

// lastSeen reads both event styles: kubelet's (last timestamp) and that of
// events.k8s.io reporters such as the scheduler (event time and series).
func lastSeen(e corev1.Event) time.Time {
	t := e.LastTimestamp.Time
	if e.EventTime.After(t) {
		t = e.EventTime.Time
	}
	if e.Series != nil && e.Series.LastObservedTime.After(t) {
		t = e.Series.LastObservedTime.Time
	}
	if t.IsZero() {
		t = e.FirstTimestamp.Time
	}
	return t
}

// logs prints the last lines of a pod's container.
func (r *Reader) logs(ctx context.Context, req protocol.KubeReadRequest) (string, error) {
	if req.Namespace == "" || req.Name == "" {
		return "", protocol.Errorf(protocol.CodeBadRequest, "logs need the namespace and name of a pod")
	}
	if req.Kind != "" {
		if res, err := r.resolve(req.Kind); err != nil || res.gvr.Resource != "pods" || res.gvr.Group != "" {
			return "", protocol.Errorf(protocol.CodeBadRequest, "logs are read from pods: list the pods of a %s first", req.Kind)
		}
	}
	pod, err := r.Kube.CoreV1().Pods(req.Namespace).Get(ctx, req.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	var names []string
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	var initNames []string
	for _, c := range pod.Spec.InitContainers {
		initNames = append(initNames, c.Name)
	}
	container := req.Container
	if container == "" && len(names) > 0 {
		container = names[0]
	}
	if !slices.Contains(names, container) && !slices.Contains(initNames, container) {
		return "", protocol.Errorf(protocol.CodeBadRequest, "pod %s has no container %q; containers: %s%s",
			req.Name, container, strings.Join(names, ", "), initList(initNames))
	}
	tail := int64(req.TailLines)
	if tail <= 0 {
		tail = defaultTail
	}
	tail = min(tail, maxTail)
	limit := int64(4 * MaxText)
	raw, err := r.Kube.CoreV1().Pods(req.Namespace).GetLogs(req.Name, &corev1.PodLogOptions{
		Container: container, Previous: req.Previous, TailLines: &tail, LimitBytes: &limit,
	}).DoRaw(ctx)
	if err != nil {
		return "", kube.LogsError(err, raw)
	}
	run := "current run"
	if req.Previous {
		run = "previous run"
	}
	head := fmt.Sprintf("Logs of pod %s, container %s, %s, last %d lines", req.Name, container, run, tail)
	if len(names) > 1 {
		head += "; containers: " + strings.Join(names, ", ")
	}
	head += initList(initNames)
	body := strings.TrimRight(string(raw), "\n")
	if body == "" {
		body = "(no lines)"
	}
	return head + ":\n" + body, nil
}

func initList(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "; init containers: " + strings.Join(names, ", ")
}
