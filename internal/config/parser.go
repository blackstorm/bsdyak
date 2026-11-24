package config

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var rootSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "project", LabelNames: []string{"name"}},
		{Type: "target", LabelNames: []string{"name"}},
		{Type: "storage", LabelNames: []string{"name"}},
		{Type: "image", LabelNames: []string{"name"}},
		{Type: "service", LabelNames: []string{"name"}},
	},
}

var projectSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "freebsd", Required: true},
	},
}

var targetSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "host", Required: true},
		{Name: "default"},
		{Name: "vars"},
	},
}

var storageSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "size"},
		{Name: "snapshot_before_deploy"},
		{Name: "zfs"},
	},
}

var imageSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "stage", LabelNames: []string{"name"}},
		{Type: "runtime"},
	},
}

var stageSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "on"},
		{Name: "packages"},
		{Name: "cache"},
		{Name: "workdir"},
		{Name: "env"},
		{Name: "build_secrets"},
		{Name: "steps", Required: true},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "copy"},
	},
}

var runtimeSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "packages"},
		{Name: "setup"},
		{Name: "env"},
		{Name: "user"},
		{Name: "workdir"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "copy"},
	},
}

var copySchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "from"},
		{Name: "src", Required: true},
		{Name: "dst", Required: true},
	},
}

var serviceSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "image", Required: true},
		{Name: "depends_on"},
		{Name: "strategy"},
		{Name: "stop_grace"},
		{Name: "jail"},
		{Name: "env"},
		{Name: "init_cmd"},
		{Name: "migrate"},
		{Name: "cmd", Required: true},
		{Name: "port"},
	},
	Blocks: []hcl.BlockHeaderSchema{
		{Type: "mount"},
		{Type: "healthcheck"},
		{Type: "expose"},
		{Type: "resources"},
		{Type: "hook"},
	},
}

var mountSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "source", Required: true},
		{Name: "target", Required: true},
		{Name: "owner"},
	},
}

var healthcheckSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "http"},
		{Name: "cmd"},
		{Name: "interval"},
		{Name: "timeout"},
	},
}

var exposeSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "listen"},
		{Name: "unix"},
		{Name: "to", Required: true},
	},
}

var resourcesSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "memory"},
		{Name: "cpu"},
	},
}

var hookSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "on_switch"},
	},
}

// ParseFile parses, links, and validates an HCL Yakfile (with any included files).
func ParseFile(yakfilePath, targetName string) (*Config, error) {
	mergedBody, files, diags := loadMergedFiles(yakfilePath)
	if diags.HasErrors() {
		return nil, formatDiagnostics(diags, files)
	}

	rootContent, cDiags := mergedBody.Content(rootSchema)
	diags = append(diags, cDiags...)
	if diags.HasErrors() {
		return nil, formatDiagnostics(diags, files)
	}

	cfg := &Config{
		Targets:  make(map[string]Target),
		Storage:  make(map[string]Storage),
		Images:   make(map[string]Image),
		Services: make(map[string]Service),
	}

	// Pass 1: discover symbol tables + targets + secret references.
	secretRefs := make(map[string]bool)
	for _, f := range files {
		if sb, ok := f.Body.(*hclsyntax.Body); ok {
			collectSecretRefsFromSyntax(sb, secretRefs)
		}
	}

	var projectBlocks, targetBlocks, storageBlocks, imageBlocks, serviceBlocks []*hcl.Block
	for _, blk := range rootContent.Blocks {
		switch blk.Type {
		case "project":
			projectBlocks = append(projectBlocks, blk)
		case "target":
			targetBlocks = append(targetBlocks, blk)
		case "storage":
			storageBlocks = append(storageBlocks, blk)
		case "image":
			imageBlocks = append(imageBlocks, blk)
		case "service":
			serviceBlocks = append(serviceBlocks, blk)
		}
	}

	if len(projectBlocks) != 1 {
		return nil, fmt.Errorf("Yakfile must declare exactly 1 `project` block (found %d)", len(projectBlocks))
	}

	// Decode targets first (with nil eval context) so var.* can be resolved for selected target.
	for _, blk := range targetBlocks {
		tName := blk.Labels[0]
		if _, exists := cfg.Targets[tName]; exists {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Duplicate target block",
				Detail:   fmt.Sprintf("target %q already defined", tName),
				Subject:  blk.DefRange.Ptr(),
			})
			continue
		}
		tc, tDiags := blk.Body.Content(targetSchema)
		diags = append(diags, tDiags...)
		if tDiags.HasErrors() {
			continue
		}
		tgt := Target{Name: tName}
		diags = append(diags, gohcl.DecodeExpression(tc.Attributes["host"].Expr, nil, &tgt.Host)...)
		if a, ok := tc.Attributes["default"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, nil, &tgt.Default)...)
		}
		if a, ok := tc.Attributes["vars"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, nil, &tgt.Vars)...)
		}
		cfg.Targets[tName] = tgt
	}
	if diags.HasErrors() {
		return nil, formatDiagnostics(diags, files)
	}

	selected, err := resolveTarget(cfg.Targets, targetName)
	if err != nil {
		return nil, err
	}
	cfg.SelectedTarget = selected

	// Build Pass 2 evaluation context.
	storageMap := make(map[string]cty.Value)
	for _, blk := range storageBlocks {
		name := blk.Labels[0]
		storageMap[name] = cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal(name),
		})
	}
	imageMap := make(map[string]cty.Value)
	for _, blk := range imageBlocks {
		name := blk.Labels[0]
		imageMap[name] = cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal(name),
		})
	}
	serviceMap := make(map[string]cty.Value)
	for _, blk := range serviceBlocks {
		name := blk.Labels[0]
		serviceMap[name] = cty.ObjectVal(map[string]cty.Value{
			"name": cty.StringVal(name),
			"host": cty.StringVal(name),
		})
	}
	secretMap := make(map[string]cty.Value)
	var refKeys []string
	for k := range secretRefs {
		refKeys = append(refKeys, k)
		secretMap[k] = cty.StringVal(fmt.Sprintf("${secret:%s}", k))
	}
	sort.Strings(refKeys)
	cfg.ReferencedSecrets = refKeys

	varMap := make(map[string]cty.Value)
	if selected != nil {
		for k, v := range selected.Vars {
			varMap[k] = cty.StringVal(v)
		}
	}

	evalCtx := &hcl.EvalContext{
		Variables: map[string]cty.Value{
			"storage": mapToObj(storageMap),
			"image":   mapToObj(imageMap),
			"service": mapToObj(serviceMap),
			"secret":  mapToObj(secretMap),
			"var":     mapToObj(varMap),
		},
	}

	// Pass 2: Decode project, storage, image, service blocks.
	{
		pBlk := projectBlocks[0]
		pc, pDiags := pBlk.Body.Content(projectSchema)
		diags = append(diags, pDiags...)
		if !pDiags.HasErrors() {
			cfg.Project.Name = pBlk.Labels[0]
			diags = append(diags, gohcl.DecodeExpression(pc.Attributes["freebsd"].Expr, evalCtx, &cfg.Project.FreeBSD)...)
		}
	}

	for _, blk := range storageBlocks {
		name := blk.Labels[0]
		if _, exists := cfg.Storage[name]; exists {
			diags = append(diags, dupDiag("storage", name, blk.DefRange))
			continue
		}
		sc, sDiags := blk.Body.Content(storageSchema)
		diags = append(diags, sDiags...)
		if sDiags.HasErrors() {
			continue
		}
		st := Storage{Name: name}
		if a, ok := sc.Attributes["size"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, evalCtx, &st.Size)...)
		}
		if a, ok := sc.Attributes["snapshot_before_deploy"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, evalCtx, &st.SnapshotBeforeDeploy)...)
		}
		if a, ok := sc.Attributes["zfs"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, evalCtx, &st.ZFS)...)
		}
		cfg.Storage[name] = st
	}

	for _, blk := range imageBlocks {
		name := blk.Labels[0]
		if _, exists := cfg.Images[name]; exists {
			diags = append(diags, dupDiag("image", name, blk.DefRange))
			continue
		}
		img, iDiags := decodeImage(name, blk, evalCtx)
		diags = append(diags, iDiags...)
		cfg.Images[name] = img
	}

	for _, blk := range serviceBlocks {
		name := blk.Labels[0]
		if _, exists := cfg.Services[name]; exists {
			diags = append(diags, dupDiag("service", name, blk.DefRange))
			continue
		}
		svc, sDiags := decodeService(name, blk, evalCtx)
		diags = append(diags, sDiags...)
		cfg.Services[name] = svc
	}

	if diags.HasErrors() {
		return nil, formatDiagnostics(diags, files)
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeImage(name string, blk *hcl.Block, parentCtx *hcl.EvalContext) (Image, hcl.Diagnostics) {
	img := Image{Name: name}
	ic, diags := blk.Body.Content(imageSchema)
	if diags.HasErrors() {
		return img, diags
	}

	// Build local stage.<name> reference table scoped to this image.
	stageMap := make(map[string]cty.Value)
	var stageBlocks, runtimeBlocks []*hcl.Block
	for _, b := range ic.Blocks {
		switch b.Type {
		case "stage":
			sName := b.Labels[0]
			stageMap[sName] = cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal(sName)})
			stageBlocks = append(stageBlocks, b)
		case "runtime":
			runtimeBlocks = append(runtimeBlocks, b)
		}
	}
	if len(runtimeBlocks) != 1 {
		diags = append(diags, &hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Missing or duplicate runtime block",
			Detail:   fmt.Sprintf("image %q must contain exactly 1 `runtime` block (found %d)", name, len(runtimeBlocks)),
			Subject:  blk.DefRange.Ptr(),
		})
		return img, diags
	}

	imgCtx := parentCtx.NewChild()
	imgCtx.Variables = map[string]cty.Value{
		"stage": mapToObj(stageMap),
	}

	seenStages := make(map[string]bool)
	for _, sb := range stageBlocks {
		sName := sb.Labels[0]
		if seenStages[sName] {
			diags = append(diags, dupDiag("stage", sName, sb.DefRange))
			continue
		}
		seenStages[sName] = true
		sc, sDiags := sb.Body.Content(stageSchema)
		diags = append(diags, sDiags...)
		if sDiags.HasErrors() {
			continue
		}
		st := Stage{Name: sName, On: "server"}
		if a, ok := sc.Attributes["on"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &st.On)...)
		}
		if a, ok := sc.Attributes["packages"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &st.Packages)...)
		}
		if a, ok := sc.Attributes["cache"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &st.Cache)...)
		}
		if a, ok := sc.Attributes["workdir"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &st.Workdir)...)
		}
		if a, ok := sc.Attributes["env"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &st.Env)...)
		}
		if a, ok := sc.Attributes["build_secrets"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &st.BuildSecrets)...)
		}
		diags = append(diags, gohcl.DecodeExpression(sc.Attributes["steps"].Expr, imgCtx, &st.Steps)...)

		for _, cb := range sc.Blocks {
			cp, cpDiags := decodeCopy(cb, imgCtx)
			diags = append(diags, cpDiags...)
			st.Copy = append(st.Copy, cp)
		}
		img.Stages = append(img.Stages, st)
	}

	rc, rDiags := runtimeBlocks[0].Body.Content(runtimeSchema)
	diags = append(diags, rDiags...)
	if !rDiags.HasErrors() {
		if a, ok := rc.Attributes["packages"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &img.Runtime.Packages)...)
		}
		if a, ok := rc.Attributes["setup"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &img.Runtime.Setup)...)
		}
		if a, ok := rc.Attributes["env"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &img.Runtime.Env)...)
		}
		if a, ok := rc.Attributes["user"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &img.Runtime.User)...)
		}
		if a, ok := rc.Attributes["workdir"]; ok {
			diags = append(diags, gohcl.DecodeExpression(a.Expr, imgCtx, &img.Runtime.Workdir)...)
		}
		for _, cb := range rc.Blocks {
			cp, cpDiags := decodeCopy(cb, imgCtx)
			diags = append(diags, cpDiags...)
			img.Runtime.Copy = append(img.Runtime.Copy, cp)
		}
	}

	return img, diags
}

func decodeCopy(cb *hcl.Block, ctx *hcl.EvalContext) (CopySpec, hcl.Diagnostics) {
	var cp CopySpec
	cc, diags := cb.Body.Content(copySchema)
	if diags.HasErrors() {
		return cp, diags
	}
	if a, ok := cc.Attributes["from"]; ok {
		ref, rDiags := decodeRefName(a.Expr, ctx)
		diags = append(diags, rDiags...)
		cp.FromStage = ref
	}
	diags = append(diags, gohcl.DecodeExpression(cc.Attributes["src"].Expr, ctx, &cp.Src)...)
	diags = append(diags, gohcl.DecodeExpression(cc.Attributes["dst"].Expr, ctx, &cp.Dst)...)
	return cp, diags
}

func decodeService(name string, blk *hcl.Block, ctx *hcl.EvalContext) (Service, hcl.Diagnostics) {
	svc := Service{Name: name}
	sc, diags := blk.Body.Content(serviceSchema)
	if diags.HasErrors() {
		return svc, diags
	}

	imgRef, iDiags := decodeRefName(sc.Attributes["image"].Expr, ctx)
	diags = append(diags, iDiags...)
	svc.Image = imgRef

	if a, ok := sc.Attributes["depends_on"]; ok {
		deps, dDiags := decodeRefList(a.Expr, ctx)
		diags = append(diags, dDiags...)
		svc.DependsOn = deps
	}
	if a, ok := sc.Attributes["strategy"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.Strategy)...)
	}
	if a, ok := sc.Attributes["stop_grace"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.StopGrace)...)
	}
	if a, ok := sc.Attributes["jail"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.Jail)...)
	}
	if a, ok := sc.Attributes["env"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.Env)...)
	}
	if a, ok := sc.Attributes["init_cmd"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.InitCmd)...)
	}
	if a, ok := sc.Attributes["migrate"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.Migrate)...)
	}
	diags = append(diags, gohcl.DecodeExpression(sc.Attributes["cmd"].Expr, ctx, &svc.Cmd)...)
	if a, ok := sc.Attributes["port"]; ok {
		diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &svc.Port)...)
	}

	for _, b := range sc.Blocks {
		switch b.Type {
		case "mount":
			mc, mDiags := b.Body.Content(mountSchema)
			diags = append(diags, mDiags...)
			if mDiags.HasErrors() {
				continue
			}
			var m Mount
			srcRef, sDiags := decodeRefName(mc.Attributes["source"].Expr, ctx)
			diags = append(diags, sDiags...)
			m.Source = srcRef
			diags = append(diags, gohcl.DecodeExpression(mc.Attributes["target"].Expr, ctx, &m.Target)...)
			if a, ok := mc.Attributes["owner"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &m.Owner)...)
			}
			svc.Mounts = append(svc.Mounts, m)
		case "healthcheck":
			hc, hDiags := b.Body.Content(healthcheckSchema)
			diags = append(diags, hDiags...)
			if hDiags.HasErrors() {
				continue
			}
			var h Healthcheck
			if a, ok := hc.Attributes["http"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &h.HTTP)...)
			}
			if a, ok := hc.Attributes["cmd"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &h.Cmd)...)
			}
			if a, ok := hc.Attributes["interval"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &h.Interval)...)
			}
			if a, ok := hc.Attributes["timeout"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &h.Timeout)...)
			}
			svc.Healthcheck = &h
		case "expose":
			ec, eDiags := b.Body.Content(exposeSchema)
			diags = append(diags, eDiags...)
			if eDiags.HasErrors() {
				continue
			}
			var e Expose
			if a, ok := ec.Attributes["listen"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &e.Listen)...)
			}
			if a, ok := ec.Attributes["unix"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &e.Unix)...)
			}
			diags = append(diags, gohcl.DecodeExpression(ec.Attributes["to"].Expr, ctx, &e.To)...)
			svc.Expose = &e
		case "resources":
			rc, rDiags := b.Body.Content(resourcesSchema)
			diags = append(diags, rDiags...)
			if rDiags.HasErrors() {
				continue
			}
			var r Resources
			if a, ok := rc.Attributes["memory"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &r.Memory)...)
			}
			if a, ok := rc.Attributes["cpu"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &r.CPU)...)
			}
			svc.Resources = &r
		case "hook":
			hkc, hkDiags := b.Body.Content(hookSchema)
			diags = append(diags, hkDiags...)
			if hkDiags.HasErrors() {
				continue
			}
			var hk Hook
			if a, ok := hkc.Attributes["on_switch"]; ok {
				diags = append(diags, gohcl.DecodeExpression(a.Expr, ctx, &hk.OnSwitch)...)
			}
			svc.Hook = &hk
		}
	}

	if svc.Strategy == "" {
		if svc.Expose != nil {
			svc.Strategy = "blue-green"
		} else {
			svc.Strategy = "recreate"
		}
	}
	return svc, diags
}

func resolveTarget(targets map[string]Target, name string) (*Target, error) {
	if name != "" {
		t, ok := targets[name]
		if !ok {
			return nil, fmt.Errorf("target %q is not defined in Yakfile", name)
		}
		return &t, nil
	}
	var defaults []Target
	for _, t := range targets {
		if t.Default {
			defaults = append(defaults, t)
		}
	}
	if len(defaults) > 1 {
		return nil, fmt.Errorf("multiple target blocks specify `default = true`")
	}
	if len(defaults) == 1 {
		t := defaults[0]
		return &t, nil
	}
	if len(targets) == 1 {
		for _, t := range targets {
			cp := t
			return &cp, nil
		}
	}
	return nil, nil
}

func collectSecretRefsFromSyntax(b *hclsyntax.Body, refs map[string]bool) {
	for _, attr := range b.Attributes {
		for _, trav := range attr.Expr.Variables() {
			if len(trav) >= 2 && trav.RootName() == "secret" {
				if step, ok := trav[1].(hcl.TraverseAttr); ok {
					refs[step.Name] = true
				}
			}
		}
	}
	for _, blk := range b.Blocks {
		collectSecretRefsFromSyntax(blk.Body, refs)
	}
}

func decodeRefName(expr hcl.Expression, ctx *hcl.EvalContext) (string, hcl.Diagnostics) {
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return "", diags
	}
	return extractRefName(val, expr.Range())
}

func decodeRefList(expr hcl.Expression, ctx *hcl.EvalContext) ([]string, hcl.Diagnostics) {
	val, diags := expr.Value(ctx)
	if diags.HasErrors() {
		return nil, diags
	}
	if !val.Type().IsTupleType() && !val.Type().IsListType() && !val.Type().IsSetType() {
		return nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Expected list of service references",
			Subject:  expr.Range().Ptr(),
		}}
	}
	var out []string
	for it := val.ElementIterator(); it.Next(); {
		_, elem := it.Element()
		name, nDiags := extractRefName(elem, expr.Range())
		diags = append(diags, nDiags...)
		out = append(out, name)
	}
	return out, diags
}

func extractRefName(val cty.Value, rng hcl.Range) (string, hcl.Diagnostics) {
	if val.Type() == cty.String {
		return val.AsString(), nil
	}
	if val.Type().IsObjectType() && val.Type().HasAttribute("name") {
		nv := val.GetAttr("name")
		if nv.Type() == cty.String {
			return nv.AsString(), nil
		}
	}
	return "", hcl.Diagnostics{{
		Severity: hcl.DiagError,
		Summary:  "Invalid reference value",
		Detail:   "expected a block reference (e.g. image.backend, storage.uploads, service.db) or string",
		Subject:  rng.Ptr(),
	}}
}

func mapToObj(m map[string]cty.Value) cty.Value {
	if len(m) == 0 {
		return cty.EmptyObjectVal
	}
	return cty.ObjectVal(m)
}

func dupDiag(kind, name string, rng hcl.Range) *hcl.Diagnostic {
	return &hcl.Diagnostic{
		Severity: hcl.DiagError,
		Summary:  fmt.Sprintf("Duplicate %s block", kind),
		Detail:   fmt.Sprintf("%s %q is already defined", kind, name),
		Subject:  rng.Ptr(),
	}
}

func formatDiagnostics(diags hcl.Diagnostics, files map[string]*hcl.File) error {
	var buf bytes.Buffer
	writer := hcl.NewDiagnosticTextWriter(&buf, files, 100, false)
	_ = writer.WriteDiagnostics(diags)
	return fmt.Errorf("%s", strings.TrimSpace(buf.String()))
}
