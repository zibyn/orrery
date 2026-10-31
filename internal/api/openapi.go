package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/zibyn/orrery/internal/httpjson"
)

// OpenAPIDocument is generated from the same route table that serves requests,
// so the document cannot drift from the handlers (ticket #47).
func (s *Server) OpenAPIDocument() map[string]any {
	paths := map[string]any{}
	for _, rt := range s.routes() {
		key := "/api/v1" + rt.Path
		item, _ := paths[key].(map[string]any)
		if item == nil {
			item = map[string]any{}
			paths[key] = item
		}
		op := map[string]any{
			"operationId": rt.OperationID,
			"summary":     rt.Summary,
			"tags":        []string{tagFor(rt.Path)},
			"responses":   responsesFor(rt),
		}
		var params []any
		for _, name := range pathParams(rt.Path) {
			schema := map[string]any{"type": "string", "format": "uuid"}
			if name == "port" {
				schema = map[string]any{"type": "integer", "minimum": 1, "maximum": 65535}
			}
			if name == "name" || name == "provider_id" ||
				(name == "id" && (strings.HasPrefix(rt.Path, "/git-providers/") || strings.HasPrefix(rt.Path, "/external-auth/"))) {
				schema = map[string]any{"type": "string"}
			}
			params = append(params, map[string]any{
				"name": name, "in": "path", "required": true,
				"schema": schema,
			})
		}
		for _, name := range rt.Query {
			schema := map[string]any{"type": "string"}
			if name == "limit" {
				schema = map[string]any{"type": "integer", "minimum": 1, "maximum": maxPageSize}
			}
			params = append(params, map[string]any{
				"name": name, "in": "query", "required": false, "schema": schema,
			})
		}
		if len(params) > 0 {
			op["parameters"] = params
		}
		if rt.Request != "" {
			op["requestBody"] = map[string]any{
				"required": true,
				"content":  map[string]any{"application/json": map[string]any{"schema": schemaRef(rt.Request)}},
			}
		}
		item[strings.ToLower(rt.Method)] = op
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "Orrery API",
			"version":     "1.0.0",
			"description": "Generated from the server route table. docs/spec/api.md is the semantic authority.",
		},
		"paths":      paths,
		"components": map[string]any{"schemas": schemas()},
	}
}

func schemaRef(name string) map[string]any {
	return map[string]any{"$ref": "#/components/schemas/" + name}
}

func tagFor(path string) string {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		return "root"
	}
	return parts[0]
}

func pathParams(path string) []string {
	var names []string
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(segment, "{"), "}"))
		}
	}
	return names
}

func responsesFor(rt route) map[string]any {
	success := map[string]any{"description": "Success"}
	if rt.Response != "" {
		success["content"] = map[string]any{"application/json": map[string]any{"schema": schemaRef(rt.Response)}}
	}
	responses := map[string]any{strconv.Itoa(rt.SuccessStatus): success}
	if rt.SuccessStatus == http.StatusFound {
		success["description"] = "Redirect"
		success["headers"] = map[string]any{
			"Location": map[string]any{"schema": map[string]any{"type": "string"}},
		}
	}
	if rt.SuccessStatus == http.StatusSwitchingProtocols {
		success["description"] = "Switching Protocols. The WebSocket subprotocol is sandbox.v1."
		if rt.OperationID == "attachExecPTY" {
			success["description"] = "Switching Protocols. The WebSocket subprotocol is sandbox.v1; binary frames carry terminal bytes and text frames carry controls."
		}
		responses["503"] = problemResponse("unavailable")
	}
	for _, status := range []string{"400", "401", "403", "404", "409", "500"} {
		responses[status] = problemResponse(problemCodeFor(status))
	}
	if rt.OperationID == "createSession" {
		responses["429"] = map[string]any{
			"description": httpjson.Title("rate_limited"),
			"headers": map[string]any{
				"Retry-After": map[string]any{"schema": map[string]any{"type": "integer", "description": "Seconds until another attempt is allowed."}},
			},
			"content": map[string]any{"application/problem+json": map[string]any{"schema": schemaRef("Problem")}},
		}
	}
	switch rt.OperationID {
	case "putSecret":
		// PUT upserts: 201 when the Secret is new, 200 when it is overwritten.
		responses["201"] = map[string]any{
			"description": "Created",
			"content":     map[string]any{"application/json": map[string]any{"schema": schemaRef(rt.Response)}},
		}
		responses["413"] = problemResponse("payload_too_large")
		responses["429"] = problemResponse("quota_exceeded")
		responses["503"] = problemResponse("unavailable")
	case "listSecrets", "deleteSecret", "getGitCredential":
		responses["503"] = problemResponse("unavailable")
	case "createSnapshot", "createSnapshotVersion":
		// An Idempotency-Key replay returns the resource that already exists.
		responses["200"] = map[string]any{
			"description": "The Snapshot or SnapshotVersion created by an earlier request with the same Idempotency-Key",
			"content":     map[string]any{"application/json": map[string]any{"schema": schemaRef(rt.Response)}},
		}
		if rt.OperationID == "createSnapshotVersion" {
			// A build is asynchronous; an unfulfillable build ends the version
			// failed/build_failed through admission, not with 422.
			responses["503"] = problemResponse("unavailable")
		}
	case "createResourceUpload":
		responses["413"] = problemResponse("payload_too_large")
		responses["503"] = problemResponse("unavailable")
	case "createResourceVersion", "getResourceManifest", "createResourceDownloads":
		responses["503"] = problemResponse("unavailable")
	case "publishWorkspace":
		responses["503"] = problemResponse("unavailable")
	case "setResourceCurrent":
		responses["412"] = problemResponse("precondition_failed")
	}
	return responses
}

func problemResponse(code string) map[string]any {
	return map[string]any{
		"description": httpjson.Title(code),
		"content":     map[string]any{"application/problem+json": map[string]any{"schema": schemaRef("Problem")}},
	}
}

func problemCodeFor(status string) string {
	n, _ := strconv.Atoi(status)
	return httpjson.CodeForStatus(n)
}

func schemas() map[string]any {
	nullableString := map[string]any{"type": []string{"string", "null"}}
	nullableTime := map[string]any{"type": []string{"string", "null"}, "format": "date-time"}
	return map[string]any{
		"Problem": map[string]any{
			"type":     "object",
			"required": []string{"type", "title", "status", "code"},
			"properties": map[string]any{
				"type":   map[string]any{"type": "string"},
				"title":  map[string]any{"type": "string"},
				"status": map[string]any{"type": "integer"},
				"detail": map[string]any{"type": "string"},
				"code": map[string]any{"type": "string", "enum": []string{
					"invalid_request", "unauthenticated", "permission_denied", "not_found",
					"conflict", "precondition_failed", "payload_too_large", "unsupported",
					"quota_exceeded", "rate_limited", "internal", "unavailable",
				}},
			},
		},
		"Quota": map[string]any{
			"type":     "object",
			"required": []string{"gpus", "sandboxes"},
			"properties": map[string]any{
				"gpus":      map[string]any{"type": "integer", "minimum": 0},
				"sandboxes": map[string]any{"type": "integer", "minimum": 0},
			},
		},
		"User": map[string]any{
			"type":     "object",
			"required": []string{"id", "name", "is_admin", "login_type", "quota", "created_at"},
			"properties": map[string]any{
				"id":         map[string]any{"type": "string", "format": "uuid"},
				"name":       map[string]any{"type": "string"},
				"email":      nullableString,
				"is_admin":   map[string]any{"type": "boolean"},
				"login_type": map[string]any{"type": "string", "enum": []string{"password", "oidc", "none"}},
				"quota":      schemaRef("Quota"),
				"created_at": map[string]any{"type": "string", "format": "date-time"},
				"deleted_at": nullableTime,
			},
		},
		"UserList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("User")},
				"next_cursor": nullableString,
			},
		},
		"CreateUserRequest": map[string]any{
			"type":     "object",
			"required": []string{"name", "login_type"},
			"properties": map[string]any{
				"name":       map[string]any{"type": "string"},
				"email":      nullableString,
				"login_type": map[string]any{"type": "string", "enum": []string{"password", "oidc", "none"}},
				"password":   nullableString,
				"is_admin":   map[string]any{"type": "boolean"},
			},
		},
		"UpdateUserRequest": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":       map[string]any{"type": "string"},
				"email":      nullableString,
				"login_type": map[string]any{"type": "string", "enum": []string{"password", "oidc", "none"}},
				"password":   nullableString,
				"is_admin":   map[string]any{"type": "boolean"},
			},
		},
		"SetQuotaRequest": map[string]any{
			"type":     "object",
			"required": []string{"gpus", "sandboxes"},
			"properties": map[string]any{
				"gpus":      map[string]any{"type": "integer", "minimum": 0},
				"sandboxes": map[string]any{"type": "integer", "minimum": 0},
			},
		},
		"QuotaUsageItem": map[string]any{
			"type":     "object",
			"required": []string{"limit", "used"},
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer"},
				"used":  map[string]any{"type": "integer"},
			},
		},
		"UserQuotaUsage": map[string]any{
			"type":     "object",
			"required": []string{"gpus", "sandboxes"},
			"properties": map[string]any{
				"gpus":      schemaRef("QuotaUsageItem"),
				"sandboxes": schemaRef("QuotaUsageItem"),
			},
		},
		"Token": map[string]any{
			"type":     "object",
			"required": []string{"id", "kind", "name", "scope", "created_at"},
			"properties": map[string]any{
				"id":           map[string]any{"type": "string", "format": "uuid"},
				"kind":         map[string]any{"type": "string", "enum": []string{"token", "session"}},
				"name":         map[string]any{"type": "string"},
				"scope":        map[string]any{"type": "string", "enum": []string{"read_only", "user", "admin"}},
				"expires_at":   nullableTime,
				"last_used_at": nullableTime,
				"created_at":   map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"CreatedToken": map[string]any{
			"allOf": []any{
				schemaRef("Token"),
				map[string]any{
					"type":       "object",
					"required":   []string{"token"},
					"properties": map[string]any{"token": map[string]any{"type": "string", "description": "Plaintext, shown only in this response."}},
				},
			},
		},
		"TokenList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Token")},
				"next_cursor": nullableString,
			},
		},
		"CreateTokenRequest": map[string]any{
			"type":     "object",
			"required": []string{"name", "scope"},
			"properties": map[string]any{
				"name":       map[string]any{"type": "string"},
				"scope":      map[string]any{"type": "string", "enum": []string{"read_only", "user", "admin"}},
				"expires_at": nullableTime,
			},
		},
		"SessionRequest": map[string]any{
			"type":     "object",
			"required": []string{"username", "password"},
			"properties": map[string]any{
				"username": map[string]any{"type": "string"},
				"password": map[string]any{"type": "string"},
				"token":    schemaRef("CreateTokenRequest"),
			},
		},
		"Secret": map[string]any{
			"type":     "object",
			"required": []string{"name", "updated_at"},
			"properties": map[string]any{
				"name":       map[string]any{"type": "string", "description": "The value is never returned."},
				"updated_at": map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"SecretList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Secret")},
				"next_cursor": nullableString,
			},
		},
		"PutSecretRequest": map[string]any{
			"type":     "object",
			"required": []string{"value"},
			"properties": map[string]any{
				"value": map[string]any{"type": "string", "maxLength": maxSecretBytes,
					"description": "At most 24 KiB of UTF-8 bytes."},
			},
		},
		"GitProvider": map[string]any{
			"type":     "object",
			"required": []string{"id", "type", "host"},
			"properties": map[string]any{
				"id":   map[string]any{"type": "string", "description": "The id used in /git-providers/{id}/authorize."},
				"type": map[string]any{"type": "string", "enum": []string{"github", "gitlab", "gitea", "generic"}},
				"host": map[string]any{"type": "string", "description": "Pattern matching repository hosts."},
			},
		},
		"GitProviderList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("GitProvider")},
				"next_cursor": nullableString,
			},
		},
		"GitLink": map[string]any{
			"type":     "object",
			"required": []string{"provider_id", "linked_at"},
			"properties": map[string]any{
				"provider_id": map[string]any{"type": "string"},
				"linked_at":   map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"GitLinkList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("GitLink")},
				"next_cursor": nullableString,
			},
		},
		"GitCredentialRequest": map[string]any{
			"type":        "object",
			"description": "Exactly one of host or provider.",
			"properties": map[string]any{
				"host":     map[string]any{"type": "string", "description": "Repository host, matched against the providers' host patterns."},
				"provider": map[string]any{"type": "string", "description": "A provider id from GET /git-providers."},
			},
		},
		"GitCredential": map[string]any{
			"type":     "object",
			"required": []string{"username", "password"},
			"properties": map[string]any{
				"username": map[string]any{"type": "string"},
				"password": map[string]any{"type": "string"},
			},
		},
		"ChangePasswordRequest": map[string]any{
			"type":     "object",
			"required": []string{"old", "new"},
			"properties": map[string]any{
				"old": map[string]any{"type": "string"},
				"new": map[string]any{"type": "string", "minLength": minPasswordLen},
			},
		},
		"LoginResponse": map[string]any{
			"oneOf": []any{schemaRef("Token"), schemaRef("CreatedToken")},
			"description": "Token is the browser login shape (the Session travels in Set-Cookie). " +
				"CreatedToken is returned when the request asked for a named API Token.",
		},
		"ResetPasswordRequest": map[string]any{
			"type":     "object",
			"required": []string{"password"},
			"properties": map[string]any{
				"password": map[string]any{"type": "string", "minLength": minPasswordLen},
			},
		},
		"AuditRecord": map[string]any{
			"type":     "object",
			"required": []string{"id", "at", "action", "target_type", "target_id", "detail"},
			"properties": map[string]any{
				"id":             map[string]any{"type": "integer"},
				"at":             map[string]any{"type": "string", "format": "date-time"},
				"actor_user_id":  nullableString,
				"actor_name":     nullableString,
				"actor_token_id": nullableString,
				"action":         map[string]any{"type": "string"},
				"target_type":    map[string]any{"type": "string"},
				"target_id":      map[string]any{"type": "string"},
				"correlation_id": nullableString,
				"detail":         map[string]any{"type": "object"},
			},
		},
		"AuditList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("AuditRecord")},
				"next_cursor": nullableString,
			},
		},
		"EnrollmentPacket": map[string]any{
			"type":     "object",
			"required": []string{"id", "packet", "expires_at"},
			"properties": map[string]any{
				"id":         map[string]any{"type": "string", "format": "uuid"},
				"packet":     map[string]any{"type": "string", "description": "Plaintext, shown only in this response. Single-use."},
				"expires_at": map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"GPU": map[string]any{
			"type":     "object",
			"required": []string{"uuid", "model"},
			"properties": map[string]any{
				"uuid":  map[string]any{"type": "string"},
				"model": map[string]any{"type": "string"},
			},
		},
		"Node": map[string]any{
			"type":     "object",
			"required": []string{"id", "name", "status", "paused", "gpus", "created_at"},
			"properties": map[string]any{
				"id":             map[string]any{"type": "string", "format": "uuid"},
				"name":           map[string]any{"type": "string"},
				"status":         map[string]any{"type": "string", "enum": []string{"ready", "paused", "unavailable", "outdated", "lost"}},
				"paused":         map[string]any{"type": "boolean"},
				"lost_at":        nullableTime,
				"agent_version":  nullableString,
				"proto_major":    map[string]any{"type": []string{"integer", "null"}},
				"proto_minor":    map[string]any{"type": []string{"integer", "null"}},
				"last_seen_at":   nullableTime,
				"runtime_health": nullableString,
				"capabilities":   map[string]any{"type": []string{"object", "null"}},
				"hardware":       map[string]any{"type": []string{"object", "null"}},
				"gpus":           map[string]any{"type": "array", "items": schemaRef("GPU")},
				"created_at":     map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"NodeList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Node")},
				"next_cursor": nullableString,
			},
		},
		"CapacityGPU": map[string]any{
			"type":     "object",
			"required": []string{"model", "total", "allocated"},
			"properties": map[string]any{
				"model":     map[string]any{"type": "string"},
				"total":     map[string]any{"type": "integer"},
				"allocated": map[string]any{"type": "integer"},
			},
		},
		"CapacityNode": map[string]any{
			"type":     "object",
			"required": []string{"node_id", "name", "status", "gpus"},
			"properties": map[string]any{
				"node_id": map[string]any{"type": "string", "format": "uuid"},
				"name":    map[string]any{"type": "string"},
				"status":  map[string]any{"type": "string", "enum": []string{"ready", "paused", "unavailable", "outdated", "lost"}},
				"gpus":    map[string]any{"type": "array", "items": schemaRef("CapacityGPU")},
			},
		},
		"CapacityList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("CapacityNode")},
				"next_cursor": nullableString,
			},
		},
		"SandboxWorkspace": map[string]any{
			"type":     "object",
			"required": []string{"id", "mode"},
			"properties": map[string]any{
				"id":   map[string]any{"type": "string", "format": "uuid"},
				"mode": map[string]any{"type": "string", "enum": []string{"rw", "ro"}},
			},
		},
		"Workspace": map[string]any{
			"type":     "object",
			"required": []string{"id", "kind", "state", "unavailable", "conditions", "created_at"},
			"properties": map[string]any{
				"id":          map[string]any{"type": "string", "format": "uuid"},
				"kind":        map[string]any{"type": "string", "enum": []string{"ephemeral", "persistent"}},
				"node_id":     nullableString,
				"state":       map[string]any{"type": "string", "enum": workspaceStateNames},
				"cause":       nullableString,
				"unavailable": map[string]any{"type": "boolean", "description": "Derived from Node reachability, never stored."},
				"conditions":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"sandbox_id":  nullableString,
				"created_at":  map[string]any{"type": "string", "format": "date-time"},
				"deleted_at":  nullableTime,
			},
		},
		"WorkspaceList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Workspace")},
				"next_cursor": nullableString,
			},
		},
		"Snapshot": map[string]any{
			"type":     "object",
			"required": []string{"id", "name", "created_at"},
			"properties": map[string]any{
				"id":                 map[string]any{"type": "string", "format": "uuid"},
				"owner_id":           map[string]any{"type": []string{"string", "null"}, "format": "uuid", "description": "Null for a platform Snapshot."},
				"name":               map[string]any{"type": "string"},
				"current_version_id": map[string]any{"type": []string{"string", "null"}, "format": "uuid"},
				"created_at":         map[string]any{"type": "string", "format": "date-time"},
				"deleted_at":         nullableTime,
			},
		},
		"SnapshotList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Snapshot")},
				"next_cursor": nullableString,
			},
		},
		"CreateSnapshotRequest": map[string]any{
			"type":     "object",
			"required": []string{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string"},
			},
		},
		"SnapshotVersion": map[string]any{
			"type":     "object",
			"required": []string{"id", "snapshot_id", "state", "source", "created_at"},
			"properties": map[string]any{
				"id":               map[string]any{"type": "string", "format": "uuid"},
				"snapshot_id":      map[string]any{"type": "string", "format": "uuid"},
				"digest":           nullableString,
				"state":            map[string]any{"type": "string", "enum": snapshotVersionStateNames},
				"cause":            nullableString,
				"source":           map[string]any{"type": "string", "enum": []string{"import", "build"}},
				"build_sandbox_id": nullableString,
				"build_exec_id":    nullableString,
				"provenance":       map[string]any{"type": []string{"object", "null"}},
				"created_at":       map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"SnapshotVersionList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("SnapshotVersion")},
				"next_cursor": nullableString,
			},
		},
		"CreateSnapshotVersionRequest": map[string]any{
			"type":        "object",
			"description": "Exactly one of import or build.",
			"properties": map[string]any{
				"import": map[string]any{
					"type":     "object",
					"required": []string{"ref"},
					"properties": map[string]any{
						"ref":         map[string]any{"type": "string", "description": "ref@sha256:…; the whole index is copied preserving its digest."},
						"auth_secret": map[string]any{"type": "string", "description": "Name of the caller's Secret holding a Docker config.json for the source."},
					},
				},
				"build": schemaRef("BuildRequest"),
			},
		},
		"BuildRequest": map[string]any{
			"type":        "object",
			"description": "Build a Snapshot on a Node. The build runs with system priority, holds an Allocation, does not count toward User Quota, and is bounded by the deployment build CPU/memory/timeout limits.",
			"required":    []string{"dockerfile"},
			"properties": map[string]any{
				"dockerfile": map[string]any{"type": "string", "description": "Dockerfile text. There is no default build context."},
				"base":       map[string]any{"type": "string", "description": "SnapshotVersion id or Snapshot name (own first, then platform), injected as the named context base."},
				"contexts": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Named context to resource[@version]; resolved to concrete versions at create time.",
				},
				"secrets": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "Docker secret id to Secret name. A build with Secrets runs without the build cache.",
				},
			},
		},
		"SetSnapshotCurrentRequest": map[string]any{
			"type":     "object",
			"required": []string{"version"},
			"properties": map[string]any{
				"version": map[string]any{"type": "string", "format": "uuid"},
			},
		},
		"CreateSandboxRequest": map[string]any{
			"type":     "object",
			"required": []string{"cpu", "memory"},
			"properties": map[string]any{
				"name":   map[string]any{"type": "string", "description": "Unique per User; defaults to the Sandbox id."},
				"labels": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"runtime": map[string]any{
					"type":        "string",
					"enum":        []string{"container", "local"},
					"default":     "container",
					"description": "container places on a containerd Node; local places on a Local Runtime Node and accepts no Snapshot or Resources.",
				},
				"workspace": map[string]any{
					"type":        "object",
					"description": "Attach an existing Workspace; omit to create an implicit ephemeral one.",
					"required":    []string{"id"},
					"properties": map[string]any{
						"id":   map[string]any{"type": "string", "format": "uuid"},
						"mode": map[string]any{"type": "string", "enum": []string{"rw", "ro"}, "default": "rw"},
					},
				},
				"snapshot": map[string]any{"type": "string", "description": "SnapshotVersion id or Snapshot name (own first, then platform); omit for the deployment default."},
				"resources": map[string]any{
					"type":        "array",
					"description": "Resource versions pinned at create time and mounted read-only at /resources/<mount>.",
					"items": map[string]any{
						"type":     "object",
						"required": []string{"resource"},
						"properties": map[string]any{
							"resource": map[string]any{"type": "string", "description": "Resource id or owner-scoped name."},
							"version":  map[string]any{"type": "string", "format": "uuid", "description": "ResourceVersion id; omit for the current version."},
							"mount":    map[string]any{"type": "string", "description": "One path segment; defaults to the Resource name."},
						},
					},
				},
				"secrets": map[string]any{
					"type":                 "object",
					"description":          "Environment variable name to Secret name; injected into every Exec.",
					"additionalProperties": map[string]any{"type": "string"},
				},
				"cpu":    map[string]any{"type": "number", "exclusiveMinimum": 0, "description": "CPU cores."},
				"memory": map[string]any{"type": "integer", "minimum": 1, "description": "Bytes of memory."},
				"gpu": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"count": map[string]any{"type": "integer", "minimum": 0, "description": "Whole GPUs; bounded by the User Quota."},
						"model": map[string]any{"type": "string"},
					},
					"required": []string{"count"},
				},
				"idle_stop": map[string]any{"type": "integer", "minimum": 0, "default": 900, "description": "Seconds; 0 disables idle stop."},
				"egress":    schemaRef("Egress"),
				"links":     schemaRef("SandboxLinks"),
			},
		},
		"SandboxLinks": map[string]any{
			"type":        "object",
			"description": "Sandbox Links, declared on the target and fixed for its life: the Sandboxes in from may connect to it on ports, outside their Egress Policy. Everything else that connects to the target is dropped.",
			"required":    []string{"from"},
			"properties": map[string]any{
				"from": map[string]any{
					"type":        "array",
					"minItems":    1,
					"maxItems":    maxLinkEntries,
					"items":       map[string]any{"type": "string", "format": "uuid"},
					"description": "The caller's own undeleted Sandboxes; none may be runtime local.",
				},
				"ports": map[string]any{
					"type":        "array",
					"maxItems":    maxLinkEntries,
					"items":       map[string]any{"type": "integer", "minimum": 1, "maximum": 65535},
					"description": "Omitted means every TCP and UDP port.",
				},
			},
		},
		"Egress": map[string]any{
			"type":        "object",
			"description": "Egress Mode, fixed for the Sandbox's life and narrowing only what the deployment's Egress Rules permit. Omitted means open. A mode other than open is placed only on Nodes where egress_policy is enforced.",
			"required":    []string{"mode"},
			"properties": map[string]any{
				"mode": map[string]any{"type": "string", "enum": []string{"open", "none", "allowlist"}},
				"allow": map[string]any{
					"type":        "array",
					"minItems":    1,
					"maxItems":    maxEgressAllow,
					"items":       map[string]any{"type": "string", "description": "IPv4 CIDR (a bare address is /32) with an optional :port."},
					"description": "Required and non-empty for allowlist; rejected with any other mode.",
				},
			},
		},
		"Sandbox": map[string]any{
			"type":     "object",
			"required": []string{"id", "name", "labels", "runtime", "egress", "state", "snapshot", "conditions", "cpu", "memory", "idle_stop", "created_at"},
			"properties": map[string]any{
				"id":       map[string]any{"type": "string", "format": "uuid"},
				"name":     map[string]any{"type": "string"},
				"labels":   map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"runtime":  map[string]any{"type": "string", "enum": []string{"container", "local"}},
				"egress":   schemaRef("Egress"),
				"links":    schemaRef("SandboxLinks"),
				"address":  map[string]any{"type": "string", "description": "Current IPv4 address; changes on every start, empty while not running."},
				"state":    map[string]any{"type": "string", "enum": sandboxStateNames},
				"cause":    nullableString,
				"node_id":  nullableString,
				"snapshot": map[string]any{"type": "string"},
				"cpu":      map[string]any{"type": "number"},
				"memory":   map[string]any{"type": "integer"},
				"gpu": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"count": map[string]any{"type": "integer"},
						"model": map[string]any{"type": "string"},
					},
					"required": []string{"count"},
				},
				"idle_stop":      map[string]any{"type": "integer"},
				"conditions":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Current conditions, e.g. fetching_resources while the Node pulls Resource version trees."},
				"workspace":      schemaRef("SandboxWorkspace"),
				"queue_position": map[string]any{"type": "integer", "description": "1-based position in the admission queue while pending."},
				"blocked_reason": nullableString,
				"created_at":     map[string]any{"type": "string", "format": "date-time"},
			},
		},
		"SandboxList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Sandbox")},
				"next_cursor": nullableString,
			},
		},
		"CreateExecRequest": map[string]any{
			"type":     "object",
			"required": []string{"argv"},
			"properties": map[string]any{
				"argv":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1, "description": "The program and its arguments; no shell."},
				"env":     map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}},
				"cwd":     map[string]any{"type": "string", "description": "Relative to /workspace."},
				"timeout": map[string]any{"type": "integer", "minimum": 1, "description": "Seconds; omit for no timeout."},
				"stdin":   map[string]any{"type": "string", "description": "Written once, then closed."},
				"pty": map[string]any{
					"type":        "object",
					"description": "Present requests a PTY. The output is not stored; attach at GET /execs/{id}/pty. stdin is ignored.",
					"properties": map[string]any{
						"rows": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535},
						"cols": map[string]any{"type": "integer", "minimum": 0, "maximum": 65535},
					},
				},
			},
		},
		"ExecResult": map[string]any{
			"type":     "object",
			"required": []string{"cause"},
			"properties": map[string]any{
				"cause":     map[string]any{"type": "string", "enum": execCauseNames},
				"exit_code": map[string]any{"type": "integer"},
				"signal":    map[string]any{"type": "integer"},
			},
		},
		"Exec": map[string]any{
			"type":     "object",
			"required": []string{"id", "sandbox_id", "argv", "state", "log_truncated", "accepted_at"},
			"properties": map[string]any{
				"id":            map[string]any{"type": "string", "format": "uuid"},
				"sandbox_id":    map[string]any{"type": "string", "format": "uuid"},
				"argv":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"cwd":           map[string]any{"type": "string"},
				"timeout":       map[string]any{"type": []string{"integer", "null"}},
				"state":         map[string]any{"type": "string", "enum": []string{"pending", "running", "canceling", "finished"}},
				"result":        map[string]any{"anyOf": []any{schemaRef("ExecResult"), map[string]any{"type": "null"}}},
				"log_truncated": map[string]any{"type": "boolean"},
				"accepted_at":   map[string]any{"type": "string", "format": "date-time"},
				"started_at":    nullableTime,
				"finished_at":   nullableTime,
				"deadline_at":   nullableTime,
			},
		},
		"ExecList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Exec")},
				"next_cursor": nullableString,
			},
		},
		"ExecOutputFrame": map[string]any{
			"type":     "object",
			"required": []string{"seq", "stream", "data"},
			"properties": map[string]any{
				"seq":      map[string]any{"type": "integer", "description": "The frame's byte offset in the Node record; pass the end of the last frame as after."},
				"stream":   map[string]any{"type": "string", "enum": []string{"stdout", "stderr", "truncated"}},
				"data":     map[string]any{"type": "string"},
				"encoding": map[string]any{"type": "string", "enum": []string{"base64"}, "description": "Present when data is not valid UTF-8."},
			},
		},
		"ExecOutput": map[string]any{
			"type":        "array",
			"items":       schemaRef("ExecOutputFrame"),
			"description": "A page of frames. With follow=true the same frames arrive as WebSocket text messages, ending with a {type: \"result\"} frame.",
		},
		"FileEntry": map[string]any{
			"type":     "object",
			"required": []string{"path", "name", "type", "size", "mode", "mod_time"},
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "Relative to /workspace."},
				"name":        map[string]any{"type": "string"},
				"type":        map[string]any{"type": "string", "enum": []string{"file", "dir", "symlink", "other"}},
				"size":        map[string]any{"type": "integer"},
				"mode":        map[string]any{"type": "integer", "description": "Permission bits."},
				"mod_time":    map[string]any{"type": "string", "format": "date-time"},
				"link_target": map[string]any{"type": "string", "description": "Present for a symlink."},
			},
		},
		"FileList": map[string]any{
			"type":     "object",
			"required": []string{"items"},
			"properties": map[string]any{
				"items": map[string]any{"type": "array", "items": schemaRef("FileEntry")},
			},
		},
		"FileMkdirRequest": map[string]any{
			"type":     "object",
			"required": []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		},
		"FileMoveRequest": map[string]any{
			"type":     "object",
			"required": []string{"from", "to"},
			"properties": map[string]any{
				"from": map[string]any{"type": "string"},
				"to":   map[string]any{"type": "string"},
			},
		},
		"PortTicket": map[string]any{
			"type":     "object",
			"required": []string{"url"},
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "Signed URL on the port's subdomain; valid for 60 seconds."},
			},
		},
		"CreateResourceRequest": map[string]any{
			"type":     "object",
			"required": []string{"name"},
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Unique per User among undeleted Resources."},
			},
		},
		"Resource": map[string]any{
			"type":     "object",
			"required": []string{"id", "name", "created_at"},
			"properties": map[string]any{
				"id":                 map[string]any{"type": "string", "format": "uuid"},
				"name":               map[string]any{"type": "string"},
				"current_version_id": nullableString,
				"created_at":         map[string]any{"type": "string", "format": "date-time"},
				"deleted_at":         nullableTime,
			},
		},
		"ResourceList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("Resource")},
				"next_cursor": nullableString,
			},
		},
		"Manifest": map[string]any{
			"type":     "object",
			"required": []string{"entries"},
			"properties": map[string]any{
				"entries": map[string]any{"type": "array", "items": schemaRef("ManifestEntry"), "maxItems": 1000000},
			},
		},
		"ManifestEntry": map[string]any{
			"type":     "object",
			"required": []string{"path", "type"},
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "description": "Slash-separated relative path; the parent must be an explicit dir entry."},
				"type":       map[string]any{"type": "string", "enum": []string{"file", "dir", "symlink"}},
				"size":       map[string]any{"type": "integer", "minimum": 0, "maximum": 5 << 30, "description": "File only; required for files, at most 5 GiB."},
				"sha256":     map[string]any{"type": "string", "description": "File only; 64 lowercase hex characters."},
				"executable": map[string]any{"type": "boolean", "description": "File only; the sole recorded permission bit (644 or 755)."},
				"target":     map[string]any{"type": "string", "description": "Symlink only; recorded, never followed."},
			},
		},
		"Upload": map[string]any{
			"type":     "object",
			"required": []string{"upload_id", "expires_at", "puts"},
			"properties": map[string]any{
				"upload_id":  map[string]any{"type": "string", "format": "uuid"},
				"expires_at": map[string]any{"type": "string", "format": "date-time"},
				"puts":       map[string]any{"type": "array", "items": schemaRef("UploadPut")},
			},
		},
		"UploadPut": map[string]any{
			"type":     "object",
			"required": []string{"sha256", "url"},
			"properties": map[string]any{
				"sha256": map[string]any{"type": "string"},
				"url":    map[string]any{"type": "string", "description": "Presigned PUT; send x-amz-checksum-sha256 (base64) with the bytes."},
			},
		},
		"CreateResourceVersionRequest": map[string]any{
			"type":     "object",
			"required": []string{"upload_id"},
			"properties": map[string]any{
				"upload_id": map[string]any{"type": "string", "format": "uuid"},
			},
		},
		"PublishWorkspaceRequest": map[string]any{
			"type":     "object",
			"required": []string{"path", "resource"},
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "Relative to the Workspace root; /workspace/... is accepted. Must stay inside the Workspace.",
				},
				"resource": map[string]any{
					"type":        "string",
					"description": "Resource name; created when absent.",
				},
			},
		},
		"ResourceVersion": map[string]any{
			"type":     "object",
			"required": []string{"id", "resource_id", "size", "state", "provenance", "created_at"},
			"properties": map[string]any{
				"id":          map[string]any{"type": "string", "format": "uuid"},
				"resource_id": map[string]any{"type": "string", "format": "uuid"},
				"address":     nullableString,
				"size":        map[string]any{"type": "integer"},
				"state":       map[string]any{"type": "string", "enum": resourceVersionStateNames},
				"cause":       nullableString,
				"provenance":  map[string]any{"type": "object", "description": "upload, or workspace{id, path}."},
				"created_at":  map[string]any{"type": "string", "format": "date-time"},
				"expires_at":  nullableTime,
			},
		},
		"ResourceVersionList": map[string]any{
			"type":     "object",
			"required": []string{"items", "next_cursor"},
			"properties": map[string]any{
				"items":       map[string]any{"type": "array", "items": schemaRef("ResourceVersion")},
				"next_cursor": nullableString,
			},
		},
		"ResourceDownloadsRequest": map[string]any{
			"type":     "object",
			"required": []string{"sha256"},
			"properties": map[string]any{
				"sha256": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		},
		"ResourceDownloads": map[string]any{
			"type":     "object",
			"required": []string{"urls"},
			"properties": map[string]any{
				"urls": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Presigned GETs in request order."},
			},
		},
		"SetResourceCurrentRequest": map[string]any{
			"type":     "object",
			"required": []string{"version"},
			"properties": map[string]any{
				"version": map[string]any{"type": "string", "format": "uuid"},
			},
		},
	}
}
