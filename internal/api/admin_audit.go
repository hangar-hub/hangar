package api

import (
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/httpx"
)

// idMaxCSVEvents bounds one CSV export; narrow the time range for more.
const idMaxCSVEvents = 100_000

func idAuditFilter(params gen.AdminListAuditParams) audit.Filter {
	f := audit.Filter{ActorUsername: idStr(params.Actor), Limit: idLimit(params.Limit)}
	if params.Action != nil && *params.Action != "" {
		for _, a := range strings.Split(*params.Action, ",") {
			if a = strings.TrimSpace(a); a != "" {
				f.Actions = append(f.Actions, audit.Action(a))
			}
		}
	}
	if params.Since != nil {
		f.Since = *params.Since
	}
	if params.Until != nil {
		f.Until = *params.Until
	}
	if params.Cursor != nil {
		f.Cursor = *params.Cursor
	}
	return f
}

func idAuditEventOut(e audit.Entry) gen.AuditEvent {
	out := gen.AuditEvent{Id: e.ID, Action: string(e.Action), Actor: idStrPtr(e.ActorUsername), TargetType: idStrPtr(e.TargetType),
		TargetId: idStrPtr(e.TargetID), Ip: idStrPtr(e.IP), UserAgent: idStrPtr(e.UserAgent), CreatedAt: e.Time}
	if len(e.Data) > 0 {
		d := e.Data
		out.Data = &d
	}
	return out
}

// AdminListAudit implements GET /admin/audit, as JSON pages or, with
// format=csv, as a CSV download of every matching event.
func (h *Identity) AdminListAudit(w http.ResponseWriter, r *http.Request, params gen.AdminListAuditParams) {
	if _, ok := h.instanceAdmin(w, r); !ok {
		return
	}
	f := idAuditFilter(params)
	if params.Format != nil && *params.Format == gen.Csv {
		h.writeAuditCSV(w, r, f)
		return
	}
	list, next, err := h.audit.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := gen.AuditEventList{Items: make([]gen.AuditEvent, 0, len(list)), NextCursor: idNextCursor(next)}
	for _, e := range list {
		out.Items = append(out.Items, idAuditEventOut(e))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func (h *Identity) writeAuditCSV(w http.ResponseWriter, r *http.Request, f audit.Filter) {
	f.Cursor, f.Limit = "", audit.MaxLimit
	// Read the first page before writing headers, so errors are problems.
	page, next, err := h.audit.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="hangar-audit-`+time.Now().UTC().Format("20060102T150405Z")+`.csv"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"id", "created_at", "action", "actor", "target_type", "target_id", "ip", "user_agent", "data"})
	written := 0
	for {
		for _, e := range page {
			data := ""
			if len(e.Data) > 0 {
				b, _ := json.Marshal(e.Data)
				data = string(b)
			}
			_ = cw.Write([]string{e.ID, e.Time.UTC().Format(time.RFC3339Nano), string(e.Action), idCSVSafe(e.ActorUsername),
				idCSVSafe(e.TargetType), idCSVSafe(e.TargetID), idCSVSafe(e.IP), idCSVSafe(e.UserAgent), idCSVSafe(data)})
			written++
		}
		if next == "" || written >= idMaxCSVEvents {
			break
		}
		f.Cursor = next
		page, next, err = h.audit.List(r.Context(), f)
		if err != nil {
			h.log.ErrorContext(r.Context(), "audit CSV export stopped", "error", err)
			break
		}
	}
	cw.Flush()
}

// idCSVSafe stops spreadsheet formula injection: cells that start with =,
// +, - or @ get a leading apostrophe.
func idCSVSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
