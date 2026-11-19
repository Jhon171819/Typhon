package typhon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

type openAPIObject map[string]any

type webRouteDoc struct {
	id        string
	tags      []string
	responses map[int]*validationSchema
	body      *validationSchema
	query     *validationSchema
}

type webDocsConfig struct {
	title, version string
	serve          bool
}

func (r *webRouter) document(vm *VM, args []Value) error {
	if r.frozen {
		return errors.New("router was already started")
	}
	method, pattern, id := args[0].Str, args[1].Str, args[2].Str
	route := r.routes[[2]string{method, pattern}]
	if route == nil || route.doc != nil || !validImportIdentifier(id) || len(id) > 80 {
		return errors.New("documentation requires a registered route and an operation identifier")
	}
	switch method {
	case "GET", "PUT", "POST", "DELETE", "OPTIONS", "HEAD", "PATCH", "TRACE":
	default:
		return errors.New("HTTP method is not available in OpenAPI 3.1")
	}
	for _, entry := range r.routes {
		if entry.doc != nil && entry.doc.id == id {
			return errors.New("duplicate OpenAPI operation identifier")
		}
	}
	d := &webRouteDoc{id: id, responses: map[int]*validationSchema{}}
	if len(*args[3].List) > 16 || len(args[4].Dict.Entries) == 0 || len(args[4].Dict.Entries) > 32 {
		return errors.New("invalid OpenAPI tags or response count")
	}
	seenTags := map[string]bool{}
	for _, tag := range *args[3].List {
		if !validAuthText(tag.Str, 80) || seenTags[tag.Str] {
			return errors.New("OpenAPI tags must be unique non-empty names")
		}
		seenTags[tag.Str] = true
		d.tags = append(d.tags, tag.Str)
	}
	for _, entry := range args[4].Dict.Entries {
		status := entry.Key.Int
		if status < 200 || status > 599 {
			return errors.New("OpenAPI response status must be between 200 and 599")
		}
		d.responses[int(status)] = nil
		if entry.Value.Kind != ValueNone {
			if status == 204 || status == 304 || method == http.MethodHead {
				return errors.New("OpenAPI response status or method has no response body")
			}
			schema, err := validationSchemaFrom(vm, entry.Value)
			if err != nil {
				return err
			}
			d.responses[int(status)] = schema
		}
	}
	var err error
	if len(args) > 5 && args[5].Kind != ValueNone {
		d.body, err = validationSchemaFrom(vm, args[5])
		if err != nil {
			return err
		}
	}
	if len(args) > 6 && args[6].Kind != ValueNone {
		d.query, err = validationSchemaFrom(vm, args[6])
		if err != nil {
			return err
		}
		if d.query.kind != "object" {
			return errors.New("OpenAPI query parameters require an object schema")
		}
	}
	route.doc = d
	return nil
}

func (r *webRouter) openAPI() ([]byte, error) {
	title, version := r.docs.title, r.docs.version
	if title == "" {
		title, version = "Typhon API", "1.0.0"
	}
	paths := map[string]openAPIObject{}
	document := openAPIObject{"openapi": "3.1.0", "info": openAPIObject{"title": title, "version": version}, "paths": paths}
	templated := map[string]string{}
	ordered := make([][2]string, 0, len(r.routes))
	for key, route := range r.routes {
		if route.doc != nil {
			ordered = append(ordered, key)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		return a[1] < b[1] || a[1] == b[1] && a[0] < b[0]
	})
	nodes := 0
	bearer := false
	for _, key := range ordered {
		method, path := key[0], key[1]
		route := r.routes[key]
		if paths[path] == nil {
			shape := path
			for _, param := range route.names {
				if param != "" {
					shape = strings.ReplaceAll(shape, "{"+param+"}", "{}")
				}
			}
			if previous := templated[shape]; previous != "" {
				return nil, fmt.Errorf("OpenAPI paths %q and %q use different names for equivalent path parameters", previous, path)
			}
			templated[shape] = path
			paths[path] = openAPIObject{}
		}
		operation, err := route.openAPIOperation(r.apiErrors, &nodes)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", method, path, err)
		}
		paths[path][strings.ToLower(method)] = operation
		bearer = bearer || route.auth != nil
	}
	if bearer {
		document["components"] = openAPIObject{"securitySchemes": openAPIObject{
			"bearerAuth": openAPIObject{"type": "http", "scheme": "bearer", "bearerFormat": "JWT"},
		}}
	}
	result, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(result) > 4<<20 {
		return nil, errors.New("generated OpenAPI document exceeds 4 MiB")
	}
	return append(result, '\n'), nil
}

func (r *webRoute) openAPIOperation(problemJSON bool, nodes *int) (openAPIObject, error) {
	d := r.doc
	responses := openAPIObject{}
	operation := openAPIObject{"operationId": d.id, "responses": responses}
	if len(d.tags) > 0 {
		operation["tags"] = d.tags
	}
	var params []openAPIObject
	for _, param := range r.names {
		if param != "" {
			params = append(params, openAPIObject{
				"in": "path", "name": param, "required": true, "schema": openAPIObject{"type": "string"},
			})
		}
	}
	if d.query != nil {
		fields := append([]schemaField(nil), d.query.fields...)
		sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })
		for _, field := range fields {
			if field.name == "" {
				return nil, errors.New("query parameter name cannot be empty")
			}
			schema, err := field.schema.openAPISchema(nodes, 0)
			if err != nil {
				return nil, err
			}
			params = append(params, openAPIObject{"in": "query", "name": field.name, "required": field.required, "schema": schema})
		}
	}
	if len(params) > 0 {
		operation["parameters"] = params
	}
	statuses := make([]int, 0, len(d.responses))
	for status := range d.responses {
		statuses = append(statuses, status)
	}
	sort.Ints(statuses)
	for _, status := range statuses {
		response := openAPIObject{"description": openAPIStatusDescription(status)}
		if body := d.responses[status]; body != nil {
			schema, err := body.openAPISchema(nodes, 0)
			if err != nil {
				return nil, err
			}
			response["content"] = openAPIMedia("application/json", schema)
		} else if _, known := webStatuses[status]; known && problemJSON {
			response["content"] = openAPIMedia("application/problem+json", openAPIProblemSchema())
		}
		responses[strconv.Itoa(status)] = response
	}
	addStatus := func(status int) {
		code := strconv.Itoa(status)
		if _, exists := responses[code]; exists {
			return
		}
		response := openAPIObject{"description": openAPIStatusDescription(status)}
		if problemJSON {
			response["content"] = openAPIMedia("application/problem+json", openAPIProblemSchema())
		}
		responses[code] = response
	}
	if d.body != nil {
		schema, err := d.body.openAPISchema(nodes, 0)
		if err != nil {
			return nil, err
		}
		operation["requestBody"] = openAPIObject{"required": true, "content": openAPIMedia("application/json", schema)}
		addStatus(400)
		addStatus(422)
	}
	if r.auth != nil {
		operation["security"] = []openAPIObject{{"bearerAuth": []string{}}}
		addStatus(401)
		addStatus(503)
	}
	if r.policy != nil || r.rate != nil {
		addStatus(403)
	}
	if r.rate != nil {
		addStatus(429)
	}
	return operation, nil
}

func openAPIStatusDescription(status int) string {
	if description := http.StatusText(status); description != "" {
		return description
	}
	return fmt.Sprintf("HTTP %d", status)
}

func openAPIMedia(media string, schema openAPIObject) openAPIObject {
	return openAPIObject{media: openAPIObject{"schema": schema}}
}

func openAPIProblemSchema() openAPIObject {
	text := openAPIObject{"type": "string"}
	return openAPIObject{
		"type": "object", "required": []string{"code", "message", "details", "request_id"}, "additionalProperties": false,
		"properties": openAPIObject{
			"code": text, "message": text, "request_id": text,
			"details": openAPIObject{"type": "array", "items": openAPIObject{
				"type": "object", "required": []string{"path", "rule", "code", "message"}, "additionalProperties": false,
				"properties": openAPIObject{"path": text, "rule": text, "code": text, "message": text},
			}},
		},
	}
}

func (s *validationSchema) openAPISchema(nodes *int, depth int) (openAPIObject, error) {
	*nodes++
	if *nodes > 8192 || depth > maxJSONDepth {
		return nil, errors.New("OpenAPI schema size or depth exceeded")
	}
	result := openAPIObject{"type": s.kind}
	switch s.kind {
	case "nullable", "array":
		item, err := s.item.openAPISchema(nodes, depth+1)
		if err != nil {
			return nil, err
		}
		if s.kind == "nullable" {
			return openAPIObject{"anyOf": []openAPIObject{item, {"type": "null"}}}, nil
		}
		result["items"], result["minItems"], result["maxItems"] = item, s.min.Int, s.max.Int
	case "text":
		result["type"], result["minLength"], result["maxLength"] = "string", s.min.Int, s.max.Int
	case "integer", "number":
		for key, bound := range map[string]Value{"minimum": s.min, "maximum": s.max} {
			switch bound.Kind {
			case ValueInt:
				result[key] = bound.Int
			case ValueFloat:
				result[key] = bound.Float
			}
		}
	case "enumeration":
		names := make([]string, 0, len(s.options))
		for name := range s.options {
			names = append(names, name)
		}
		sort.Strings(names)
		result["type"], result["enum"] = "string", names
	case "object":
		fields := openAPIObject{}
		var required []string
		for _, field := range s.fields {
			schema, err := field.schema.openAPISchema(nodes, depth+1)
			if err != nil {
				return nil, err
			}
			fields[field.name] = schema
			if field.required {
				required = append(required, field.name)
			}
		}
		if len(required) > 0 {
			sort.Strings(required)
			result["required"] = required
		}
		result["properties"], result["additionalProperties"] = fields, s.allowExtra
	}
	return result, nil
}

// OpenAPIFile executes module setup and exports an explicitly selected router.
func OpenAPIFile(ctx context.Context, path, binding string) (_ []byte, runErr error) {
	engine := NewEngineWithContext(ctx)
	engine.openAPIExport = true
	defer func() { runErr = finalizeRun(runErr, engine.Close()) }()
	module, err := engine.LoadModule(path)
	if err != nil {
		return nil, err
	}
	if err := engine.ExecuteModule(module, false); err != nil {
		return nil, err
	}
	engine.execMu.Lock()
	defer engine.execMu.Unlock()
	router, ok := routerValue(module.Globals[binding])
	if !ok {
		return nil, fmt.Errorf("global %q must contain a typhon.web.Router", binding)
	}
	return router.openAPI()
}

func (r *webRouter) docsHandler(next http.Handler) (http.Handler, error) {
	if !r.docs.serve {
		return next, nil
	}
	for _, path := range []string{"docs", "openapi.json"} {
		if r.root.match([]string{path}) != nil {
			return nil, errors.New("interactive docs require /docs and /openapi.json to be unregistered")
		}
	}
	spec, err := r.openAPI()
	if err != nil {
		return nil, err
	}
	jsonText := string(spec)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		media, body := "", ""
		switch req.URL.EscapedPath() {
		case "/docs":
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src https://cdn.jsdelivr.net; connect-src 'self'; style-src 'unsafe-inline'; img-src 'self' data:; font-src 'self' data:; base-uri 'none'; frame-ancestors 'none'")
			media, body = "text/html; charset=utf-8", webRedocHTML
		case "/openapi.json":
			media, body = "application/json; charset=utf-8", jsonText
		default:
			next.ServeHTTP(w, req)
			return
		}
		if req.Method != "GET" && req.Method != "HEAD" {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", media)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if req.Method == "GET" {
			_, _ = io.WriteString(w, body)
		}
	}), nil
}

const webRedocHTML = `<!doctype html>
<html lang="en">
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>API documentation</title>
<a href="/openapi.json">OpenAPI JSON</a>
<redoc spec-url="/openapi.json" disable-google-font></redoc>
<script src="https://cdn.jsdelivr.net/npm/redoc@2.5.3/bundles/redoc.standalone.js"
 integrity="sha384-xiEssMQFSpSfLbzRZCGfxxIM5QDb2DTrU6vyoZdp2sV1L6pmOMy6MpTtUoLbpC96" crossorigin="anonymous"></script>
</html>`

func openAPIPrint(text string) error {
	_, err := fmt.Fprintln(os.Stderr, text)
	return err
}
