package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/zibyn/stars-sandbox/internal/store"
)

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	ctx := r.Context()

	limit, ok := parseLimit(r)
	if !ok {
		writeProblem(w, "invalid_request", "limit must be between 1 and 200")
		return
	}
	var beforeID *int64
	if raw := q.Get("cursor"); raw != "" {
		n, ok := DecodeIntCursor(raw)
		if !ok {
			writeProblem(w, "invalid_request", "invalid cursor")
			return
		}
		beforeID = &n
	}

	var actorID *uuid.UUID
	if actor := q.Get("actor"); actor != "" {
		if id, err := uuid.Parse(actor); err == nil {
			actorID = &id
		} else if user, err := s.q.GetUserByName(ctx, actor); err == nil {
			actorID = &user.ID
		} else {
			writeProblem(w, "invalid_request", "actor must be a User id or name")
			return
		}
	}

	from, ok := parseTimeParam(q.Get("from"))
	if !ok {
		writeProblem(w, "invalid_request", "from must be an RFC 3339 timestamp")
		return
	}
	to, ok := parseTimeParam(q.Get("to"))
	if !ok {
		writeProblem(w, "invalid_request", "to must be an RFC 3339 timestamp")
		return
	}

	params := store.ListAuditParams{
		ActorID:  actorID,
		BeforeID: beforeID,
		FromAt:   from,
		ToAt:     to,
		Limit:    limit + 1,
	}
	if v := q.Get("action"); v != "" {
		params.Action = &v
	}
	if v := q.Get("target"); v != "" {
		params.TargetID = &v
	}
	if v := q.Get("target_type"); v != "" {
		params.TargetType = &v
	}

	rows, err := s.q.ListAudit(ctx, params)
	if err != nil {
		s.internal(w, "list audit", err)
		return
	}
	rows, next := trimPage(rows, limit, func(a store.ListAuditRow) string { return EncodeIntCursor(a.ID) })
	out := auditList{Items: make([]auditDTO, 0, len(rows)), NextCursor: next}
	for _, row := range rows {
		out.Items = append(out.Items, auditDTO{
			ID:            row.ID,
			At:            row.At.Time.UTC(),
			ActorUserID:   uuidPtr(row.ActorUserID),
			ActorName:     row.ActorName,
			ActorTokenID:  uuidPtr(row.ActorTokenID),
			Action:        row.Action,
			TargetType:    row.TargetType,
			TargetID:      row.TargetID,
			CorrelationID: row.CorrelationID,
			Detail:        row.Detail,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func parseTimeParam(raw string) (pgtype.Timestamptz, bool) {
	if raw == "" {
		return pgtype.Timestamptz{}, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return pgtype.Timestamptz{}, false
	}
	return pgtype.Timestamptz{Time: t, Valid: true}, true
}

func timestamptzPtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}
