package timing

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Span struct {
	Name string
	Dur  time.Duration
}

type Recorder struct {
	mu    sync.Mutex
	spans []Span
	attrs map[string]any
}

func (r *Recorder) Record(name string, d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spans = append(r.spans, Span{Name: name, Dur: d})
}

func (r *Recorder) Spans() []Span {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Span, len(r.spans))
	copy(out, r.spans)
	return out
}

func (r *Recorder) SetAttr(name string, value any) {
	if r == nil || name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attrs == nil {
		r.attrs = map[string]any{}
	}
	r.attrs[name] = value
}

func (r *Recorder) Attrs() map[string]any {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.attrs) == 0 {
		return nil
	}
	out := make(map[string]any, len(r.attrs))
	for k, v := range r.attrs {
		out[k] = v
	}
	return out
}

func (r *Recorder) Totals() map[string]time.Duration {
	if r == nil {
		return nil
	}
	totals := map[string]time.Duration{}
	for _, s := range r.Spans() {
		totals[s.Name] += s.Dur
	}
	return totals
}

func (r *Recorder) Summary() string {
	if r == nil {
		return ""
	}
	totals := r.Totals()
	if len(totals) == 0 {
		return ""
	}
	type kv struct {
		name string
		dur  time.Duration
	}
	list := make([]kv, 0, len(totals))
	for k, v := range totals {
		list = append(list, kv{name: k, dur: v})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].dur > list[j].dur })
	var b strings.Builder
	for i, item := range list {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%dms", item.name, item.dur.Milliseconds())
	}
	return b.String()
}

type ctxKey struct{}

func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, r)
}

func RecorderFrom(ctx context.Context) *Recorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(ctxKey{}).(*Recorder)
	return r
}

func RecordSpan(ctx context.Context, name string, d time.Duration) {
	RecorderFrom(ctx).Record(name, d)
}

func Stop(ctx context.Context, name string, start time.Time) {
	RecordSpan(ctx, name, time.Since(start))
}

func SetAttr(ctx context.Context, name string, value any) {
	RecorderFrom(ctx).SetAttr(name, value)
}
