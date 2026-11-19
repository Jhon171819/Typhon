package typhon

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const maxPageOffset = 10000

type webPaginator struct {
	cipher            cipher.AEAD
	order             string
	filters           *validationSchema
	defaultLimit, max int64
	lifetime          time.Duration
	now               func() time.Time
}

func webPaginationTypes(types map[string]Type) {
	httpTypes := httpDriverTypes()
	schema := validationTypes()["Schema"]
	page := Type{Kind: TypeNative, Name: "typhon.web.Page", Members: map[string]Type{
		"limit": IntType(), "offset": IntType(), "after": IntType(), "order": StrType(),
		"filter":  nativeFuncType("web.page.filter", []Type{StrType()}, AnyType(), 0),
		"respond": nativeFuncType("web.page.respond", []Type{AnyType(), unionType(IntType(), NoneType())}, httpTypes["Response"], 1),
	}}
	paginator := Type{Kind: TypeNative, Name: "typhon.web.Paginator", Members: map[string]Type{
		"parse":        nativeFuncType("web.paginator.parse", []Type{httpTypes["Request"]}, page, 0),
		"query_schema": nativeFuncType("web.paginator.query_schema", nil, schema, 0),
	}}
	types["Page"], types["Paginator"] = page, paginator
	types["paginator"] = nativeFuncType("web.paginator",
		[]Type{StrType(), StrType(), schema, IntType(), IntType(), IntType()}, paginator, 3)
}

func makeWebPaginator(types map[string]Type) Value {
	return driverFunc(types["paginator"], func(vm *VM, args []Value) (Value, error) {
		key, err := base64.RawURLEncoding.Strict().DecodeString(args[0].Str)
		if len(args[0].Str) != 43 || len(key) != 32 || err != nil {
			return NoneValue(), errors.New("pagination key must be 32 random bytes encoded as unpadded base64url")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return NoneValue(), errors.New("invalid pagination key")
		}
		aead, err := cipher.NewGCMWithRandomNonce(block)
		if err != nil {
			return NoneValue(), errors.New("cannot initialize pagination encryption")
		}
		p := &webPaginator{cipher: aead, order: args[1].Str, defaultLimit: 25, max: 100, lifetime: 15 * time.Minute, now: time.Now}
		p.filters, err = validationSchemaFrom(vm, args[2])
		if err != nil {
			return NoneValue(), err
		}
		if !validPermissionName(p.order) || p.filters.kind != "object" || p.filters.allowExtra || len(p.filters.fields) > 32 {
			return NoneValue(), errors.New("pagination requires an order name and a closed object of at most 32 filters")
		}
		for _, field := range p.filters.fields {
			if !validImportIdentifier(field.name) {
				return NoneValue(), errors.New("invalid pagination filter name")
			}
			switch field.name {
			case "limit", "offset", "order", "cursor":
				return NoneValue(), errors.New("pagination filter name is reserved")
			}
			switch field.schema.kind {
			case "text", "enumeration", "integer", "number", "boolean":
			default:
				return NoneValue(), errors.New("pagination filters must be scalar")
			}
		}
		if len(args) > 3 {
			p.defaultLimit = args[3].Int
		}
		if len(args) > 4 {
			p.max = args[4].Int
		}
		if len(args) > 5 {
			seconds := args[5].Int
			if seconds < 1 || seconds > 86400 {
				return NoneValue(), errors.New("pagination cursor lifetime must be between 1 and 86400 seconds")
			}
			p.lifetime = time.Duration(seconds) * time.Second
		}
		if p.defaultLimit < 1 || p.defaultLimit > p.max || p.max > 1000 {
			return NoneValue(), errors.New("pagination limits must be between 1 and 1000, with default no greater than max")
		}
		typ := types["Paginator"]
		value := scopedNativeObject(vm, typ, map[string]Value{
			"parse": driverFunc(typ.Members["parse"], func(vm *VM, args []Value) (Value, error) {
				return p.parse(vm, types["Page"], args[0])
			}),
			"query_schema": driverFunc(typ.Members["query_schema"], func(vm *VM, args []Value) (Value, error) {
				value := scopedNativeObject(vm, validationTypes()["Schema"], nil)
				value.NativeObject.data = p.querySchema()
				return value, nil
			}),
		})
		value.NativeObject.data = p
		return value, nil
	})
}

func (p *webPaginator) querySchema() *validationSchema {
	fields := append([]schemaField(nil), p.filters.fields...)
	fields = append(fields,
		schemaField{name: "limit", schema: &validationSchema{kind: "integer", min: IntValue(1), max: IntValue(p.max)}},
		schemaField{name: "offset", schema: &validationSchema{kind: "integer", min: IntValue(0), max: IntValue(maxPageOffset)}},
		schemaField{name: "order", schema: &validationSchema{kind: "enumeration", options: map[string]bool{p.order: true}}},
		schemaField{name: "cursor", schema: &validationSchema{kind: "text", min: IntValue(1), max: IntValue(128)}},
	)
	names := make(map[string]bool, len(fields))
	for _, field := range fields {
		names[field.name] = true
	}
	return &validationSchema{kind: "object", fields: fields, names: names}
}

func pageBadRequest() error { return (&webRejection{status: 400}).exception() }

func pageInteger(text string) (int64, error) {
	value, err := strconv.ParseInt(text, 10, 64)
	if err != nil || strconv.FormatInt(value, 10) != text {
		return 0, pageBadRequest()
	}
	return value, nil
}

func pageFilter(vm *VM, kind, text string) (Value, error) {
	switch kind {
	case "integer":
		n, err := pageInteger(text)
		return IntValue(n), err
	case "number":
		result, err := loadJSON(vm, text)
		if err != nil {
			var stop *executionStop
			if errors.As(err, &stop) {
				return NoneValue(), err
			}
			return NoneValue(), pageBadRequest()
		}
		return result, nil
	case "boolean":
		switch text {
		case "true":
			return BoolValue(true), nil
		case "false":
			return BoolValue(false), nil
		default:
			return NoneValue(), pageBadRequest()
		}
	default:
		return StringValue(text), nil
	}
}

func (p *webPaginator) parse(vm *VM, typ Type, request Value) (Value, error) {
	state, ok := request.NativeObject.data.(*webRequest)
	if !ok || len(state.rawQuery) > 8192 {
		return NoneValue(), pageBadRequest()
	}
	query, err := url.ParseQuery(state.rawQuery)
	if err != nil {
		return NoneValue(), pageBadRequest()
	}
	for key, values := range query {
		if len(values) != 1 || len(values[0]) > 1024 {
			return NoneValue(), pageBadRequest()
		}
		switch key {
		case "limit", "offset", "cursor", "order":
		default:
			if !p.filters.names[key] {
				return NoneValue(), pageBadRequest()
			}
		}
	}
	limit, offset := p.defaultLimit, int64(0)
	if query.Has("limit") {
		limit, err = pageInteger(query.Get("limit"))
		if err != nil || limit < 1 || limit > p.max {
			return NoneValue(), pageBadRequest()
		}
	}
	if query.Has("offset") {
		offset, err = pageInteger(query.Get("offset"))
		if err != nil || offset < 0 || offset > maxPageOffset || query.Has("cursor") {
			return NoneValue(), pageBadRequest()
		}
	}
	if query.Has("order") && query.Get("order") != p.order {
		return NoneValue(), pageBadRequest()
	}
	filters := DictValue()
	link := url.Values{}
	for _, field := range p.filters.fields {
		if !query.Has(field.name) {
			continue
		}
		text := query.Get(field.name)
		value, err := pageFilter(vm, field.schema.kind, text)
		if err != nil {
			return NoneValue(), err
		}
		if err := filters.Dict.put(StringValue(field.name), value); err != nil {
			return NoneValue(), err
		}
		link.Set(field.name, text)
	}
	check := validationRun{vm: vm}
	if err := check.value(p.filters, filters, "", 0); err != nil {
		return NoneValue(), err
	}
	if len(check.issues) > 0 {
		return NoneValue(), pageBadRequest()
	}
	binding := []byte(url.Values{
		"method": {request.NativeObject.Attrs["method"].Str},
		"path":   {state.path}, "subject": {state.principal.subject}, "tenant": {state.principal.tenant},
		"order": {p.order}, "filters": {link.Encode()},
	}.Encode())
	after := int64(0)
	if query.Has("cursor") {
		after, err = p.openCursor(query.Get("cursor"), binding)
		if err != nil {
			return NoneValue(), err
		}
	}
	link.Set("order", p.order)
	link.Set("limit", strconv.FormatInt(limit, 10))
	return scopedNativeObject(vm, typ, map[string]Value{
		"limit": IntValue(limit), "offset": IntValue(offset), "after": IntValue(after), "order": StringValue(p.order),
		"filter": driverFunc(typ.Members["filter"], func(vm *VM, args []Value) (Value, error) {
			pos, ok, err := filters.Dict.lookup(args[0])
			if err != nil || !ok {
				return NoneValue(), err
			}
			return filters.Dict.Entries[pos].Value, nil
		}),
		"respond": driverFunc(typ.Members["respond"], func(vm *VM, args []Value) (Value, error) {
			if args[0].Kind != ValueList {
				return NoneValue(), errors.New("pagination response items must be a list")
			}
			size := int64(len(*args[0].List))
			if size > limit {
				return NoneValue(), errors.New("page contains more items than its configured limit")
			}
			next, href := NoneValue(), NoneValue()
			if len(args) == 2 && args[1].Kind != ValueNone {
				if args[1].Int <= after || size == 0 {
					return NoneValue(), errors.New("next page position must advance past the current cursor")
				}
				token := p.makeCursor(args[1].Int, binding)
				q := make(url.Values, len(link)+1)
				for name, values := range link {
					q[name] = values
				}
				q.Set("cursor", token)
				next, href = StringValue(token), StringValue(state.path+"?"+q.Encode())
			}
			links := DictValue()
			_ = links.Dict.put(StringValue("next"), href)
			body := DictValue()
			for _, entry := range []DictEntry{
				{StringValue("items"), args[0]}, {StringValue("limit"), IntValue(limit)},
				{StringValue("next_cursor"), next}, {StringValue("links"), links},
			} {
				_ = body.Dict.put(entry.Key, entry.Value)
			}
			text, err := dumpJSON(vm, body)
			if err != nil {
				return NoneValue(), err
			}
			return httpResponse(vm, 200, text, http.Header{
				"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"private, no-store"},
			})
		}),
	}), nil
}

func (p *webPaginator) makeCursor(after int64, binding []byte) string {
	var seed [16]byte
	binary.BigEndian.PutUint64(seed[:8], uint64(after))
	binary.BigEndian.PutUint64(seed[8:], uint64(p.now().Add(p.lifetime).Unix()))
	return base64.RawURLEncoding.EncodeToString(p.cipher.Seal(nil, nil, seed[:], binding)) // #nosec G407 -- GCMWithRandomNonce requires nil and generates its own cryptographic nonce.
}

func (p *webPaginator) openCursor(token string, binding []byte) (int64, error) {
	expected := 16 + p.cipher.Overhead()
	if len(token) != base64.RawURLEncoding.EncodedLen(expected) {
		return 0, pageBadRequest()
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(raw) != expected {
		return 0, pageBadRequest()
	}
	position, err := p.cipher.Open(nil, nil, raw, binding)
	if err != nil {
		return 0, pageBadRequest()
	}
	after := int64(binary.BigEndian.Uint64(position[:8]))
	expires := int64(binary.BigEndian.Uint64(position[8:]))
	if after <= 0 || expires <= p.now().Unix() {
		return 0, pageBadRequest()
	}
	return after, nil
}
