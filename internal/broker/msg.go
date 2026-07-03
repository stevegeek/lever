package broker

import "fmt"

// msgTarget is a resolved, policy-approved message destination: the scion
// recipient string and the project (-g) it must be sent under ("" = the
// operator/user inbox, not project-scoped).
type msgTarget struct {
	scionTo string
	project string
}

// resolveMsgTarget applies the messaging policy and resolves `to` for caller.
// Identity-derived, config-authoritative, default-deny. The returned error
// text is the deny reason (audited alongside the recipient by the handler).
func (b *Broker) resolveMsgTarget(caller, to string) (msgTarget, error) {
	isManager := caller == b.manager
	_, isGrove := b.groves[caller]
	if !isManager && !isGrove {
		return msgTarget{}, fmt.Errorf("caller %q is not the manager or a declared grove", caller)
	}
	// user:* recipients are not project-scoped; any authenticated agent may
	// message the user/operator inbox (that is how groves report to the manager).
	if len(to) > 5 && to[:5] == "user:" {
		return msgTarget{scionTo: to, project: ""}, nil
	}
	name := to
	if len(to) > 6 && to[:6] == "agent:" {
		name = to[6:]
	}
	if name == b.manager {
		return msgTarget{scionTo: "agent:" + name, project: b.managerProject}, nil
	}
	spec, ok := b.groves[name]
	if !ok {
		return msgTarget{}, fmt.Errorf("unknown recipient %q", to)
	}
	if !isManager && caller != name && !b.groveToGrove {
		return msgTarget{}, fmt.Errorf("grove→grove messaging is disabled")
	}
	return msgTarget{scionTo: "agent:" + spec.Name, project: spec.JailProject}, nil
}

// resolveListProject resolves which project inbox caller may read. Manager:
// its own/operator inbox ("" ) or any declared grove's. Grove: its own only.
func (b *Broker) resolveListProject(caller, grove string) (string, error) {
	if caller == b.manager {
		if grove == "" {
			return "", nil
		}
		spec, ok := b.groves[grove]
		if !ok {
			return "", fmt.Errorf("unknown grove %q", grove)
		}
		return spec.JailProject, nil
	}
	spec, ok := b.groves[caller]
	if !ok {
		return "", fmt.Errorf("caller %q is not the manager or a declared grove", caller)
	}
	if grove != "" {
		return "", fmt.Errorf("a grove may only read its own inbox")
	}
	return spec.JailProject, nil
}
