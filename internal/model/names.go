package model

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrInvalidNamespace is returned when a Namespace name breaks the naming rules.
var ErrInvalidNamespace = errors.New("invalid namespace name")

// namespaceRe is the username rule from 05-auth.md: 2 to 42 characters,
// starting with a letter or digit.
var namespaceRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9\-_.]{1,41}$`)

// reservedNamespaces are refused at creation because they collide with
// Hangar's own routes (02-architecture.md "Routing" and 14-datasets.md).
var reservedNamespaces = map[string]struct{}{
	"api": {}, "assets": {}, "auth": {}, "datasets": {}, "datasets-server": {},
	"models": {}, "spaces": {}, "v2": {}, "healthz": {}, "readyz": {},
	"settings": {}, "xet-cas": {}, "v1": {}, "admin": {}, "discover": {},
	"arsenal": {}, "collections": {}, "new": {}, "login": {}, "logout": {},
	"docs": {}, "new-dataset": {},
}

// ReservedNamespaces returns the reserved Namespace names in lower case.
func ReservedNamespaces() []string {
	out := make([]string, 0, len(reservedNamespaces))
	for k := range reservedNamespaces {
		out = append(out, k)
	}
	return out
}

// IsReservedNamespace reports whether name cannot be used for a new Namespace.
// Names starting with '-' or '.' are reserved too.
func IsReservedNamespace(name string) bool {
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return true
	}
	_, ok := reservedNamespaces[strings.ToLower(name)]
	return ok
}

// ValidateNamespaceName checks the syntax of a Namespace name. It does not
// check reserved names, because existing Mirrors may use upstream names that
// match them; use ValidateNewNamespaceName when creating a Namespace.
func ValidateNamespaceName(name string) error {
	if !namespaceRe.MatchString(name) {
		return fmt.Errorf("%w: %q must be 2 to 42 characters, start with a letter or digit, and contain only letters, digits, '-', '_' and '.'", ErrInvalidNamespace, name)
	}
	return nil
}

// ValidateNewNamespaceName checks the syntax and refuses reserved names.
func ValidateNewNamespaceName(name string) error {
	if err := ValidateNamespaceName(name); err != nil {
		return err
	}
	if IsReservedNamespace(name) {
		return fmt.Errorf("%w: %q is reserved", ErrInvalidNamespace, name)
	}
	return nil
}
