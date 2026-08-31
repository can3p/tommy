package ingress

import (
	"fmt"
	"strings"

	"github.com/can3p/tommy/core/plugin"
)

// Mount registers every enabled HTTP provider of the registry, then checks that
// each declared Endpoint really resolves. Returns every problem at once.
func (i *Ingress) Mount(reg *plugin.Registry, base plugin.Deps) error {
	for _, ref := range reg.IngressRefs() {
		d := reg.DepsFor(base, ref.Plugin.Name(), ref.Provider.Name())
		ref.Provider.RegisterIngress(i.For(ref.Plugin.Name(), ref.Provider.Name()), d)
	}
	if err := i.Err(); err != nil {
		return err
	}

	for _, ref := range reg.IngressRefs() {
		owner := Route{Plugin: ref.Plugin.Name(), Provider: ref.Provider.Name()}.Owner()
		for _, e := range ref.Provider.Endpoints() {
			p, err := ParsePattern(strings.TrimSpace(e.Method + " " + e.Path))
			if err != nil {
				i.errs = append(i.errs, fmt.Errorf("ingress: %s: declared endpoint %q %q: %w", owner, e.Method, e.Path, err))
				continue
			}
			method := p.Method
			if method == "" {
				method = "GET"
			}
			if !i.Has(method, p.Concrete()) {
				i.errs = append(i.errs, fmt.Errorf(
					"ingress: %s declares endpoint %s %s but never mounts it", owner, method, e.Path))
			}
		}
	}
	return i.Err()
}
