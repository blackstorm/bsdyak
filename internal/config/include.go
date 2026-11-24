package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

var includeAttrSchema = &hcl.BodySchema{
	Attributes: []hcl.AttributeSchema{
		{Name: "include"},
	},
}

// loadMergedFiles parses yakfilePath and recursively resolves `include = [...]`
// inside the project root directory while preventing path traversal and cycles.
func loadMergedFiles(yakfilePath string) (hcl.Body, map[string]*hcl.File, hcl.Diagnostics) {
	absRootFile, err := filepath.Abs(yakfilePath)
	if err != nil {
		return nil, nil, hcl.Diagnostics{{
			Severity: hcl.DiagError,
			Summary:  "Invalid Yakfile path",
			Detail:   err.Error(),
		}}
	}
	rootDir := filepath.Dir(absRootFile)

	files := make(map[string]*hcl.File)
	visited := make(map[string]bool)
	active := make(map[string]bool)
	var bodies []hcl.Body
	var diags hcl.Diagnostics

	var loadOne func(path string, rng *hcl.Range)
	loadOne = func(absPath string, refRange *hcl.Range) {
		if active[absPath] {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Circular include detected",
				Detail:   fmt.Sprintf("file %s creates an include cycle", absPath),
				Subject:  refRange,
			})
			return
		}
		if visited[absPath] {
			return
		}

		relToRoot, rErr := filepath.Rel(rootDir, absPath)
		if rErr != nil || relToRoot == ".." || strings.HasPrefix(relToRoot, ".."+string(filepath.Separator)) {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Include path escapes project root",
				Detail:   fmt.Sprintf("included file %s must stay within %s", absPath, rootDir),
				Subject:  refRange,
			})
			return
		}

		src, err := os.ReadFile(absPath)
		if err != nil {
			diags = append(diags, &hcl.Diagnostic{
				Severity: hcl.DiagError,
				Summary:  "Failed to read HCL file",
				Detail:   fmt.Sprintf("%s: %v", absPath, err),
				Subject:  refRange,
			})
			return
		}

		file, parseDiags := hclsyntax.ParseConfig(src, absPath, hcl.Pos{Line: 1, Column: 1})
		diags = append(diags, parseDiags...)
		if file == nil {
			return
		}
		files[absPath] = file
		visited[absPath] = true

		content, remain, pDiags := file.Body.PartialContent(includeAttrSchema)
		diags = append(diags, pDiags...)
		bodies = append(bodies, remain)

		if attr, ok := content.Attributes["include"]; ok {
			var incPaths []string
			incDiags := gohcl.DecodeExpression(attr.Expr, nil, &incPaths)
			diags = append(diags, incDiags...)
			if !incDiags.HasErrors() {
				active[absPath] = true
				baseDir := filepath.Dir(absPath)
				for _, inc := range incPaths {
					target := inc
					if !filepath.IsAbs(target) {
						target = filepath.Join(baseDir, inc)
					}
					absInc, aErr := filepath.Abs(target)
					if aErr != nil {
						diags = append(diags, &hcl.Diagnostic{
							Severity: hcl.DiagError,
							Summary:  "Invalid include path",
							Detail:   aErr.Error(),
							Subject:  attr.Expr.Range().Ptr(),
						})
						continue
					}
					loadOne(absInc, attr.Expr.Range().Ptr())
				}
				delete(active, absPath)
			}
		}
	}

	loadOne(absRootFile, nil)
	if len(bodies) == 0 {
		return nil, files, diags
	}
	return hcl.MergeBodies(bodies), files, diags
}
