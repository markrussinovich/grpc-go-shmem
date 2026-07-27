/*
 *
 * Copyright 2026 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package shmsc

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// linknameRe matches a //go:linkname directive, capturing the local name and
// the optional target symbol (absent in the one-argument push form).
var linknameRe = regexp.MustCompile(`(?m)^//go:linkname\s+(\S+)(?:\s+(\S+))?`)

// TestNoInternalImports enforces the self-containment invariant that defines
// this module: no source file may import ANOTHER module's internal package
// (in particular none of google.golang.org/grpc/internal/*). This is what makes
// the plugin upstreamable and splittable into its own repository. This module's
// OWN internal packages (google.golang.org/grpc/plugin/shmsc/internal/...) are
// allowed.
//
// It also enforces that the only hidden linkage (//go:linkname) targets the Go
// runtime, never another module's package — the one thing the AST import check
// cannot see. It parses every checked-in .go file regardless of build tags, so a
// platform-specific file cannot smuggle in a forbidden dependency.
func TestNoInternalImports(t *testing.T) {
	const ownModulePrefix = "google.golang.org/grpc/plugin/shmsc/"
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		f, perr := parser.ParseFile(fset, path, src, parser.ImportsOnly)
		if perr != nil {
			return perr
		}
		for _, imp := range f.Imports {
			p, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				t.Errorf("%s: unparseable import literal %s", path, imp.Path.Value)
				continue
			}
			// A ".../internal" or ".../internal/..." path belonging to ANOTHER
			// module is forbidden; this module's own internal packages are fine.
			isInternal := p == "internal" || strings.HasSuffix(p, "/internal") || strings.Contains(p, "/internal/")
			if isInternal && !strings.HasPrefix(p, ownModulePrefix) {
				t.Errorf("%s imports forbidden internal package %q: this module must be self-contained", path, p)
			}
		}
		// //go:linkname may only target the Go runtime with an EXPLICIT two-argument
		// runtime.* target. Any other form is hidden linkage the import check above
		// cannot see: the one-argument push form exposes a local symbol for another
		// package to link by name, and a two-argument non-runtime target links
		// directly into another package — both would break self-containment.
		for _, m := range linknameRe.FindAllStringSubmatch(string(src), -1) {
			target := m[2]
			if target == "" {
				t.Errorf("%s: one-argument //go:linkname %q is not allowed; only an explicit runtime.* target is permitted", path, m[1])
				continue
			}
			if !strings.HasPrefix(target, "runtime.") {
				t.Errorf("%s: //go:linkname targets non-runtime symbol %q; only the Go runtime may be linked", path, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module sources: %v", err)
	}
}
