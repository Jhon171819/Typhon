package typhon

import (
	"errors"
	"log"
	"strings"
)

func authorizationPolicyType() Type { return Type{Kind: TypeNative, Name: "typhon.auth.Policy"} }

type webPolicy struct {
	roles   map[string]map[string]bool
	scopes  map[string]string
	actions map[string]bool
}

func validPermissionName(name string) bool {
	return len(name) > 0 && len(name) <= 80 && strings.IndexFunc(name, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == ':' || c == '_' || c == '-')
	}) < 0
}

func newWebPolicy(args []Value) (*webPolicy, error) {
	p := &webPolicy{roles: map[string]map[string]bool{}, scopes: map[string]string{}, actions: map[string]bool{}}
	if len(args[0].Dict.Entries) > 64 {
		return nil, errors.New("authorization policy exceeds 64 roles")
	}
	for _, entry := range args[0].Dict.Entries {
		role, actions := entry.Key.Str, *entry.Value.List
		if !validPermissionName(role) || len(actions) > 64 {
			return nil, errors.New("invalid authorization role or oversized permission list")
		}
		p.roles[role] = map[string]bool{}
		for _, action := range actions {
			if !validPermissionName(action.Str) {
				return nil, errors.New("invalid authorization action")
			}
			p.roles[role][action.Str] = true
			p.actions[action.Str] = true
		}
	}
	if len(args) == 2 {
		if len(args[1].Dict.Entries) > 64 {
			return nil, errors.New("authorization policy exceeds 64 scope requirements")
		}
		for _, entry := range args[1].Dict.Entries {
			if !p.actions[entry.Key.Str] || !validPermissionName(entry.Value.Str) {
				return nil, errors.New("scope requirement needs a defined action and valid scope")
			}
			p.scopes[entry.Key.Str] = entry.Value.Str
		}
	}
	return p, nil
}

func (p *webPolicy) allows(identity verifiedRequestIdentity, action string) bool {
	if identity.subject == "" || identity.tenant == "" {
		return false
	}
	if required := p.scopes[action]; required != "" && !identity.scopes[required] {
		return false
	}
	for _, role := range identity.roles {
		if p.roles[role][action] {
			return true
		}
	}
	return false
}

func logWebAuthorization(id, pattern, action string, allowed bool) {
	log.Printf("Typhon authorization id=%s route=%q action=%q allowed=%t", id, pattern, action, allowed)
}
