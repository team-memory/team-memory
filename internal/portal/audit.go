package portal

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/team-memory/team-memory/internal/memory"
	"net/http"
	"time"
)

type auditRow struct {
	memory.AuditEntry
	Client, Detail string
}
type auditView struct {
	Scope        string
	Filter       memory.AuditFilter
	Since, Until string
	Rows         []auditRow
	Names        map[uuid.UUID]string
	Limit        int
}

func (p *Portal) auditPage(w http.ResponseWriter, r *http.Request, q *req) error {
	values := r.URL.Query()
	v := auditView{Scope: r.PathValue("path"), Since: values.Get("since"), Until: values.Get("until"), Limit: memory.MaxLimit}
	v.Filter = memory.AuditFilter{Actor: values.Get("actor"), Client: values.Get("client"), Action: values.Get("action"), Outcome: values.Get("outcome"), Target: values.Get("target")}
	for _, d := range []struct {
		s   string
		dst **time.Time
		end bool
	}{{v.Since, &v.Filter.Since, false}, {v.Until, &v.Filter.Until, true}} {
		if d.s != "" {
			t := parseDate(d.s, d.end)
			if t.IsZero() {
				return &memory.ValidationError{Msg: "Dates must use YYYY-MM-DD"}
			}
			*d.dst = &t
		}
	}
	rows, err := p.o.Memory.FilteredAuditLog(r.Context(), q.p, v.Scope, v.Limit, v.Filter)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for _, e := range rows {
		b, _ := json.Marshal(e.Details["attribution"])
		var a memory.Attribution
		_ = json.Unmarshal(b, &a)
		detail, _ := json.MarshalIndent(e.Details, "", "  ")
		v.Rows = append(v.Rows, auditRow{AuditEntry: e, Client: a.Summary(), Detail: string(detail)})
		if e.ActorID != nil {
			ids = append(ids, *e.ActorID)
		}
	}
	v.Names = p.o.Memory.DisplayNames(r.Context(), q.p, ids)
	return p.show(q, "audit.html", "Audit log", v)
}
