package api

import (
	"net/http"

	"github.com/zibyn/stars-sandbox/internal/httpjson"
)

func writeProblem(w http.ResponseWriter, code, detail string) {
	httpjson.Problem(w, code, detail)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpjson.JSON(w, status, v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return httpjson.Decode(w, r, dst)
}
