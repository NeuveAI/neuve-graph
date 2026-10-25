package sem

import (
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// The relation phase's cheaper passes answer as they did before. Each
// "Before" function below is the implementation it replaced, copied verbatim
// but for the names of the replaced functions it calls; every test compares
// the two on real source and on generated text built to reach the shortcuts'
// edges: absent and present trigger words, upper case, and the non-ASCII text
// (the long s, the Kelvin sign) that a case-insensitive regexp folds.

func staticStringConstantsBefore(content string) map[string]string {
	constants := map[string]string{}
	for i := 0; i < 5; i++ {
		changed := false
		for _, match := range staticStringAssignRe.FindAllStringSubmatch(content, -1) {
			if len(match) != 3 {
				continue
			}
			value, ok := staticStringExpressionValue(match[2], constants)
			if !ok {
				continue
			}
			if constants[match[1]] == value {
				continue
			}
			constants[match[1]] = value
			changed = true
		}
		if !changed {
			break
		}
	}
	return constants
}

func jsRouterMountsBefore(block string, constants map[string]string) []jsRouterMount {
	var mounts []jsRouterMount
	for _, match := range jsRouterMountsRe.FindAllStringSubmatch(block, -1) {
		if len(match) != 4 {
			continue
		}
		prefix, ok := staticRouteExpressionValue(match[2], constants)
		if ok {
			mounts = append(mounts, jsRouterMount{Receiver: match[1], Prefix: prefix, Target: match[3]})
		}
	}
	for _, match := range jsRouterMountsKoaMountRe.FindAllStringSubmatch(block, -1) {
		if len(match) != 4 {
			continue
		}
		prefix, ok := staticRouteExpressionValue(match[2], constants)
		if ok {
			mounts = append(mounts, jsRouterMount{Receiver: match[1], Prefix: prefix, Target: match[3]})
		}
	}
	for _, match := range jsRouterMountsKoaUseRe.FindAllStringSubmatch(block, -1) {
		if len(match) == 3 {
			mounts = append(mounts, jsRouterMount{Receiver: match[1], Prefix: "/", Target: match[2]})
		}
	}
	return mounts
}

func jsRouterRoutesBefore(block string, constants map[string]string) []jsRouterRoute {
	jsHandlerExpr := `[A-Za-z_$][\w$]*(?:\.[A-Za-z_$][\w$]*)?`
	re := regexp.MustCompile(`(?i)\b([A-Za-z_$][\w$]*)\.(get|post|put|patch|delete|head|options)\s*\(\s*([^,\n)]+)(?:\s*,\s*(` + jsHandlerExpr + `))?`)
	prefixes := jsRouterConstructorPrefixesBefore(block, constants)
	var routes []jsRouterRoute
	for _, match := range re.FindAllStringSubmatch(block, -1) {
		if len(match) != 5 {
			continue
		}
		route, ok := staticRouteExpressionValue(match[3], constants)
		if ok {
			if prefix := prefixes[match[1]]; prefix != "" {
				route = joinRoutePaths(prefix, route)
			}
			routes = append(routes, jsRouterRoute{Receiver: match[1], Route: route, Handler: match[4]})
		}
	}
	return routes
}

func jsRouterConstructorPrefixesBefore(block string, constants map[string]string) map[string]string {
	prefixes := map[string]string{}
	for _, match := range jsRouterConstructorPrefixesRe.FindAllStringSubmatch(block, -1) {
		if len(match) != 3 {
			continue
		}
		prefixMatch := jsFastifyPluginMountsRe2.FindStringSubmatch(match[2])
		if len(prefixMatch) == 2 {
			prefix, ok := staticRouteExpressionValue(prefixMatch[1], constants)
			if !ok {
				continue
			}
			prefixes[match[1]] = prefix
			continue
		}
		if jsRouterConstructorPrefixesRe2.MatchString(strings.TrimSpace(match[2])) {
			if prefix, ok := staticRouteExpressionValue("prefix", constants); ok {
				prefixes[match[1]] = prefix
			}
		}
	}
	return prefixes
}

func returnFlowCallsBefore(body symbolBody, params map[string]bool) []returnFlowCall {
	stripped := body.stripped
	flows := map[string]returnFlowCall{}
	for _, name := range returnFlowCallNames(body) {
		flows[name+"\x00return_flow"] = returnFlowCall{
			Name:         name,
			Reason:       "callee return value flows into caller return value",
			EvidenceKind: "return_flow",
			Detail:       name,
			Direction:    "callee_to_caller",
		}
	}
	for _, flow := range ternaryReturnFlows(stripped) {
		flows[flow.Name+"\x00"+flow.EvidenceKind] = flow
	}
	for _, flow := range fallbackReturnFlows(stripped) {
		flows[flow.Name+"\x00"+flow.EvidenceKind] = flow
	}
	for _, flow := range expressionAssignedReturnFlowsBefore(stripped) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	assigned := map[string]string{}
	for _, match := range assignCallRe.FindAllStringSubmatchIndex(stripped, -1) {
		if len(match) != 6 {
			continue
		}
		if isExpressionAssignmentLine(stripped, match[0]) {
			continue
		}
		varName := strings.TrimPrefix(stripped[match[2]:match[3]], "$")
		callee := strings.TrimPrefix(stripped[match[4]:match[5]], "$")
		if varName != "" && callee != "" {
			assigned[varName] = callee
		}
	}
	for _, match := range returnVarRe.FindAllStringSubmatchIndex(stripped, -1) {
		if len(match) != 4 {
			continue
		}
		if followsReturnedVariable(stripped, match[1]) {
			continue
		}
		varName := strings.TrimPrefix(stripped[match[2]:match[3]], "$")
		name := assigned[varName]
		if name == "" {
			continue
		}
		flows[name+"\x00assigned_return_flow"] = returnFlowCall{
			Name:         name,
			Reason:       "callee return value assigned to local and returned by caller",
			EvidenceKind: "assigned_return_flow",
			Detail:       name + " -> " + varName,
			Direction:    "callee_to_caller",
		}
	}
	for _, match := range returnPropertyRe.FindAllStringSubmatchIndex(stripped, -1) {
		if len(match) != 6 {
			continue
		}
		if followsReturnedVariable(stripped, match[1]) {
			continue
		}
		varName := strings.TrimPrefix(stripped[match[2]:match[3]], "$")
		property := strings.TrimPrefix(stripped[match[4]:match[5]], "$")
		name := assigned[varName]
		if name == "" || property == "" {
			continue
		}
		flows[name+"\x00assigned_property_return_flow"] = returnFlowCall{
			Name:         name,
			Reason:       "callee return value assigned to local and returned through property by caller",
			EvidenceKind: "assigned_property_return_flow",
			Detail:       name + " -> " + varName + "." + property,
			Direction:    "callee_to_caller",
		}
	}
	for _, flow := range branchAssignedReturnFlows(stripped) {
		flows[flow.Name+"\x00assigned_return_flow"] = flow
	}
	for _, flow := range argumentForwardingFlows(stripped, params) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range parameterPropertyForwardingFlows(stripped, params) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range parameterPropertyAliasForwardingFlows(stripped, params) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	// One alias map, read by the five scanners below. Each derived its own from
	// the same stripped body and the same parameters, so a body was scanned for
	// alias assignments five times. Behind the same guard those scanners apply,
	// so a symbol with no parameters still costs no scan at all.
	var aliases map[string]string
	if len(params) > 0 {
		aliases = parameterAliasMap(stripped, params)
	}
	for _, flow := range aliasForwardingFlows(stripped, params, aliases) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range destructuredAliasForwardingFlows(stripped, params) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range objectFieldForwardingFlows(stripped, params, aliases) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range collectionElementForwardingFlows(stripped, params, aliases) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range callbackElementForwardingFlows(stripped, params, aliases) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	for _, flow := range directLiteralForwardingFlows(stripped, params, aliases) {
		flows[flow.Name+"\x00"+flow.EvidenceKind+"\x00"+flow.Detail] = flow
	}
	out := make([]returnFlowCall, 0, len(flows))
	for _, flow := range flows {
		out = append(out, flow)
	}
	// The comparator must be a total order over every field: `flows` is keyed by
	// name+kind+detail, so several entries can share a name and evidence kind and
	// differ only in detail (`a -> f()` vs `b -> f()` when both params are
	// forwarded to the same callee). Ordering only by name and kind would leave
	// those tied, letting Go's randomized map iteration decide which one survives
	// the later relation dedupe — the evidence payload would then vary run to run.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		if out[i].EvidenceKind != out[j].EvidenceKind {
			return out[i].EvidenceKind < out[j].EvidenceKind
		}
		if out[i].Detail != out[j].Detail {
			return out[i].Detail < out[j].Detail
		}
		if out[i].Direction != out[j].Direction {
			return out[i].Direction < out[j].Direction
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

func expressionAssignedReturnFlowsBefore(block string) []returnFlowCall {
	events := assignmentFlowEvents(block)
	if len(events) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var flows []returnFlowCall
	for _, match := range returnVarRe.FindAllStringSubmatchIndex(block, -1) {
		if len(match) != 4 {
			continue
		}
		if followsReturnedVariable(block, match[1]) {
			continue
		}
		returned := strings.TrimPrefix(block[match[2]:match[3]], "$")
		var last assignmentFlowEvent
		for _, event := range events {
			if event.Var != returned || event.Pos > match[0] || event.Pos < last.Pos {
				continue
			}
			if event.Pos == last.Pos && last.EvidenceKind != "" && event.EvidenceKind == "" {
				continue
			}
			last = event
		}
		if last.EvidenceKind == "" || len(last.Calls) == 0 {
			continue
		}
		for _, name := range last.Calls {
			key := name + "\x00" + last.EvidenceKind + "\x00" + returned
			if seen[key] {
				continue
			}
			seen[key] = true
			flows = append(flows, returnFlowCall{
				Name:         name,
				Reason:       last.Reason,
				EvidenceKind: last.EvidenceKind,
				Detail:       name + " -> " + returned,
				Direction:    "callee_to_caller",
			})
		}
	}
	sort.Slice(flows, func(i, j int) bool {
		if flows[i].Name != flows[j].Name {
			return flows[i].Name < flows[j].Name
		}
		return flows[i].Detail < flows[j].Detail
	})
	return flows
}

// relationPassCorpus is source to compare on: this package's Go source and
// the language fixtures, cut into blocks of a few dozen lines, and generated
// router and flow code, mutated.
func relationPassCorpus(t *testing.T) []string {
	t.Helper()
	var corpus []string
	paths, _ := filepath.Glob("*.go")
	more, _ := filepath.Glob("testdata/langmatrix/*/*")
	for _, path := range append(paths[:min(len(paths), 12)], more...) {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for start := 0; start < len(lines); start += 37 {
			corpus = append(corpus, strings.Join(lines[start:min(len(lines), start+61)], "\n"))
		}
	}
	snippets := []string{
		"const api = express.Router();\nconst BASE = '/v1';\nconst USERS = BASE + '/users';\napi.get(USERS, listUsers);\napi.POST('/users/:id', handlers.update);\napp.use('/api', api);\n",
		"const router = new Router({ prefix: '/admin' });\nrouter.delete('/x', remove)\napp.use(router.routes());\napp.use(mount('/m', inner.routes()));\n",
		"let r = new KoaRouter({prefix})\nconst prefix = '/p'\nr.Options('/o', opt)\n",
		"const ROUTE = `/plain`;\nserver.route(ROUTE, h)\nfastify.register(plugin, { prefix: '/plug' })\n",
		"router.po\u017ft('/longs', h)\nconst rou\u212ater = new Router({ prefix: '/k' })\n",
		"function f(a, b) {\n  const x = g(a);\n  const y = cond ? h(b) : k(a);\n  if (z) { return x; }\n  return y.value;\n}\n",
		"def run(self, items):\n    result = compute(items)\n    out = [transform(i) for i in items]\n    return result\n",
		"func Build(cfg Config) *Thing {\n\tt := newThing(cfg)\n\tv, err := load(cfg.Path)\n\tif err != nil { return nil }\n\treturn t\n}\n",
		"const A = 'a'; const B = A + 'b'; const C = B + A; const D = C;\n",
		"const D = C;\nconst C = B + A;\nconst B = A + 'b';\nconst A = 'a';\n",
		"x.use(y)\nnothing here returns\n",
	}
	rng := rand.New(rand.NewSource(13))
	words := []string{".get(", ".use(", "return ", "new ", "Router", "router", "const ", " = ", "'/a'", "f(", ")", "\n", "\u017f", "\u212a", "POST", ".route(", "prefix", "{", "}", ",", "x"}
	for _, snippet := range snippets {
		corpus = append(corpus, snippet)
		for range 60 {
			var b strings.Builder
			for _, piece := range strings.SplitAfter(snippet, " ") {
				if rng.Intn(5) == 0 {
					b.WriteString(words[rng.Intn(len(words))])
				}
				if rng.Intn(9) != 0 {
					b.WriteString(piece)
				}
			}
			corpus = append(corpus, b.String())
		}
	}
	return corpus
}

func TestStaticStringConstantsAreTheFixedPoint(t *testing.T) {
	for _, text := range relationPassCorpus(t) {
		if got, want := staticStringConstants(text), staticStringConstantsBefore(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("constants of %q:\n got %v\nwant %v", text, got, want)
		}
		if got, want := staticJSStringConstants(text), staticStringConstantsBefore(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("JS constants of %q:\n got %v\nwant %v", text, got, want)
		}
	}
}

func TestRouterExtractorsSkipOnlyTextTheyCannotMatch(t *testing.T) {
	matched := 0
	for _, text := range relationPassCorpus(t) {
		constants := staticStringConstantsBefore(text)
		mounts, wantMounts := jsRouterMounts(text, constants), jsRouterMountsBefore(text, constants)
		routes, wantRoutes := jsRouterRoutes(text, constants), jsRouterRoutesBefore(text, constants)
		prefixes, wantPrefixes := jsRouterConstructorPrefixes(text, constants), jsRouterConstructorPrefixesBefore(text, constants)
		if !reflect.DeepEqual(mounts, wantMounts) || !reflect.DeepEqual(routes, wantRoutes) || !reflect.DeepEqual(prefixes, wantPrefixes) {
			t.Fatalf("router extraction of %q:\n got %v %v %v\nwant %v %v %v", text, mounts, routes, prefixes, wantMounts, wantRoutes, wantPrefixes)
		}
		if len(wantMounts) > 0 || len(wantRoutes) > 0 || len(wantPrefixes) > 0 {
			matched++
		}
	}
	if matched == 0 {
		t.Fatal("no text in the corpus had a router to extract")
	}
	// The long s folds with s and the Kelvin sign with k: text holding them is
	// left to the regexp.
	for _, text := range []string{"app.po\u017fT('/x', h)", "const r = new \u212aoaRouter({ prefix: '/k' })"} {
		if got, want := jsRouterRoutes(text, nil), jsRouterRoutesBefore(text, nil); !reflect.DeepEqual(got, want) {
			t.Fatalf("routes of %q: got %v, want %v", text, got, want)
		}
	}
	if got := jsRouterRoutesBefore("app.po\u017fT('/x', h)", nil); len(got) == 0 {
		t.Fatal("fixture error: the regexp does not fold the long s")
	}
}

func TestReturnFlowsSkipOnlyBodiesThatReturnNoLocal(t *testing.T) {
	identifier := regexp.MustCompile(`[A-Za-z_]\w*`)
	rng := rand.New(rand.NewSource(17))
	flowed := 0
	for _, text := range relationPassCorpus(t) {
		params := map[string]bool{}
		for _, name := range identifier.FindAllString(text, 40) {
			if rng.Intn(3) == 0 {
				params[name] = true
			}
		}
		body := symbolBody{stripped: text}
		got, want := returnFlowCalls(body, params), returnFlowCallsBefore(body, params)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("return flows of %q:\n got %v\nwant %v", text, got, want)
		}
		if got, want := expressionAssignedReturnFlows(text), expressionAssignedReturnFlowsBefore(text); !reflect.DeepEqual(got, want) {
			t.Fatalf("expression-assigned return flows of %q:\n got %v\nwant %v", text, got, want)
		}
		if len(want) > 0 {
			flowed++
		}
	}
	if flowed == 0 {
		t.Fatal("no text in the corpus had a return flow")
	}
}

// The relation phase's memo answers what the derivations do, once per path,
// whichever workers ask at once.
func TestFileStringConstantsMemoIsTheDerivation(t *testing.T) {
	corpus := relationPassCorpus(t)[:200]
	memo := newFileStringConstants()
	var wait sync.WaitGroup
	for worker := range 8 {
		wait.Go(func() {
			for index, text := range corpus {
				path := filepath.Join("f", string(rune('a'+index%26)), strings.Repeat("x", index))
				if got := memo.forFile(path, text); !reflect.DeepEqual(got, staticStringConstantsBefore(text)) {
					t.Errorf("worker %d: constants of %s differ", worker, path)
				}
				if got := memo.importBindings(path, text); !reflect.DeepEqual(got, importedJavaScriptBindings(text)) {
					t.Errorf("worker %d: bindings of %s differ", worker, path)
				}
			}
		})
	}
	wait.Wait()
	var none *fileStringConstants
	if got := none.forFile("p", corpus[0]); !reflect.DeepEqual(got, staticStringConstantsBefore(corpus[0])) {
		t.Fatal("a nil memo did not derive the constants")
	}
}

// A pass that did not read a file is answered from what another pass read,
// and what it derives from no content is never kept for a later pass.
func TestFileStringConstantsKeepOnlyReadContent(t *testing.T) {
	const content = "const A = '/a';\nconst B = A + '/b';\n"
	memo := newFileStringConstants()
	if got := memo.forUnreadFile("x.js", ""); len(got) != 0 {
		t.Fatalf("constants of no content: %v", got)
	}
	want := staticStringConstantsBefore(content)
	if got := memo.forFile("x.js", content); !reflect.DeepEqual(got, want) {
		t.Fatalf("after an unread pass, the read file's constants are %v, want %v", got, want)
	}
	if got := memo.forUnreadFile("x.js", ""); !reflect.DeepEqual(got, want) {
		t.Fatalf("an unread pass after a read one got %v, want %v", got, want)
	}
}
