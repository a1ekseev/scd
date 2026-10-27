package xrayfake

import (
	"context"
	"slices"

	"github.com/xtls/xray-core/app/router"
	routercmd "github.com/xtls/xray-core/app/router/command"

	"github.com/a1ekseev/scd/internal/scd/xrayapi"
)

// Xray v26.10.21 error texts of the router (08 §3).
const (
	errRouterPrefix   = "app/router: "
	errEmptyTagName   = errRouterPrefix + "empty tag name!"
	errDuplicateRule  = errRouterPrefix + "duplicate ruleTag "
	errConfigTypeRule = errRouterPrefix + "AddRule: config type error"
)

type routingService struct {
	routercmd.UnimplementedRoutingServiceServer
	s *Server
}

func (r *routingService) ListRule(ctx context.Context, _ *routercmd.ListRuleRequest) (*routercmd.ListRuleResponse, error) {
	if err := r.s.enter(ctx, "ListRule", "ListRule"); err != nil {
		return nil, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	resp := &routercmd.ListRuleResponse{}
	for _, rl := range r.s.rules {
		resp.Rules = append(resp.Rules, &routercmd.ListRuleItem{Tag: rl.Tag, RuleTag: rl.RuleTag})
	}
	return resp, nil
}

// AddRule mirrors Router.ReloadRules: every rule must build a condition,
// a non-empty ruleTag must be unique (empty ones may repeat), and the
// update is atomic — on error the rules are unchanged. shouldAppend=false
// replaces all rules. Balancer existence is not modelled.
func (r *routingService) AddRule(ctx context.Context, req *routercmd.AddRuleRequest) (*routercmd.AddRuleResponse, error) {
	cfg, decodeErr := decodeRouterConfig(req)
	calls := []string{"AddRule"}
	if decodeErr == nil && len(cfg.GetRule()) > 0 {
		calls = calls[:0]
		for _, rr := range cfg.GetRule() {
			calls = append(calls, "AddRule:"+rr.GetRuleTag()+"->"+rr.GetTag())
		}
	}
	r.s.mu.Lock()
	r.s.calls = append(r.s.calls, calls[:len(calls)-1]...)
	r.s.mu.Unlock()
	if err := r.s.enter(ctx, "AddRule", calls[len(calls)-1]); err != nil {
		return nil, err
	}
	if decodeErr != nil {
		return nil, decodeErr
	}

	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var rules []xrayapi.Rule
	exist := map[string]bool{}
	if req.GetShouldAppend() {
		rules = slices.Clone(r.s.rules)
		for _, rl := range rules {
			exist[rl.RuleTag] = true
		}
	}
	for _, rr := range cfg.GetRule() {
		if _, err := rr.BuildCondition(); err != nil {
			return nil, xrayError(err.Error()) // already "app/router: …"
		}
		if rr.GetRuleTag() != "" && exist[rr.GetRuleTag()] {
			return nil, xrayError(errDuplicateRule + rr.GetRuleTag())
		}
		exist[rr.GetRuleTag()] = true
		rules = append(rules, xrayapi.Rule{Tag: rr.GetTag(), RuleTag: rr.GetRuleTag()})
	}
	r.s.rules = rules
	return &routercmd.AddRuleResponse{}, nil
}

func decodeRouterConfig(req *routercmd.AddRuleRequest) (*router.Config, error) {
	if err := checkTypedMessages(req.ProtoReflect()); err != nil {
		return nil, xrayError(err.Error())
	}
	inst, err := req.GetConfig().GetInstance()
	if err != nil {
		return nil, xrayError(err.Error())
	}
	cfg, ok := inst.(*router.Config)
	if !ok {
		return nil, xrayError(errConfigTypeRule)
	}
	return cfg, nil
}

func (r *routingService) RemoveRule(ctx context.Context, req *routercmd.RemoveRuleRequest) (*routercmd.RemoveRuleResponse, error) {
	tag := req.GetRuleTag()
	if err := r.s.enter(ctx, "RemoveRule", "RemoveRule:"+tag); err != nil {
		return nil, err
	}
	if tag == "" {
		return nil, xrayError(errEmptyTagName)
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.rules = slices.DeleteFunc(r.s.rules, func(rl xrayapi.Rule) bool { return rl.RuleTag == tag })
	return &routercmd.RemoveRuleResponse{}, nil
}
