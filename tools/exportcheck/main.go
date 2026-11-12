// Command exportcheck enforces a minimal exported surface for internal packages.
//
// Policy: an exported name (top-level decl, method, struct field, interface
// method) in a package under internal/ is justified only if some OTHER
// package's production (non-_test.go) code references it, or it appears on the
// exemption list (or is auto-exempt: interface satisfaction, serialization tag,
// reflect/json exposure, embedded field, generated file, go:linkname).
//
// The module is loaded with go/packages (Tests=true) for every combination of
// build tag set x GOOS (default: {"", "integration"} x {linux, darwin}, CGO off)
// and results are merged conservatively: a name used in ANY configuration
// counts as used.
//
// Usage: exportcheck [flags]   (see README.md)
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

// ---------------------------------------------------------------- types

type posKey struct {
	file string
	off  int
}

type decl struct {
	Key       string `json:"name"` // rel/pkg/path.Qualified.Name
	Pkg       string `json:"-"`
	Name      string `json:"-"` // qualified inside the package
	Kind      string `json:"kind"`
	File      string `json:"file"`
	Line      int    `json:"line"`
	Auto      string `json:"auto_exempt,omitempty"` // auto exemption reason
	ownerType string // key of owning type (methods/fields) or ""
	obj       types.Object
	pos       posKey
	embedded  bool
}

type usage struct {
	prod, test, xtest map[string]struct{}
	inProd, inTest    bool
}

func newUsage() *usage {
	return &usage{prod: map[string]struct{}{}, test: map[string]struct{}{}, xtest: map[string]struct{}{}}
}

func (u *usage) merge(o *usage) {
	for k := range o.prod {
		u.prod[k] = struct{}{}
	}
	for k := range o.test {
		u.test[k] = struct{}{}
	}
	for k := range o.xtest {
		u.xtest[k] = struct{}{}
	}
	u.inProd = u.inProd || o.inProd
	u.inTest = u.inTest || o.inTest
}

// result of analysing one build configuration.
type result struct {
	decls     map[string]*decl
	uses      map[string]*usage
	exposed   map[string]string   // type key -> reason (reflect/serialization exposure)
	implBy    map[string][]string // iface method key -> implementers in other pkgs
	satisfies map[string][]string // impl method key -> external contracts (other pkg/stdlib iface)
	inherit   map[string][]string // impl method key -> same-package iface method keys (inherit uses)
	linkname  map[string]struct{}
	loadErrs  []string
}

type config struct {
	goos string
	tags string
}

func (c config) String() string {
	t := c.tags
	if t == "" {
		t = "default"
	}
	return c.goos + "/" + t
}

// ---------------------------------------------------------------- loading

var serialPkgs = map[string]bool{
	"encoding/json": true, "encoding/xml": true, "encoding/gob": true, "encoding/asn1": true,
	"text/template": true, "html/template": true, "reflect": true,
	"gopkg.in/yaml.v2": true, "gopkg.in/yaml.v3": true, "sigs.k8s.io/yaml": true,
	"github.com/BurntSushi/toml": true, "github.com/pelletier/go-toml": true, "github.com/pelletier/go-toml/v2": true,
}

// wellKnownMethods are consumed through unnamed interfaces / reflection in the
// standard library (errors.Is/As/Unwrap, fmt, encoding/*, sort, io, http, flag...).
var wellKnownMethods = map[string]bool{
	"Error": true, "String": true, "GoString": true, "Format": true, "Unwrap": true, "Is": true, "As": true,
	"MarshalJSON": true, "UnmarshalJSON": true, "MarshalText": true, "UnmarshalText": true,
	"MarshalBinary": true, "UnmarshalBinary": true, "MarshalYAML": true, "UnmarshalYAML": true,
	"MarshalXML": true, "UnmarshalXML": true, "Scan": true, "Value": true,
	"Len": true, "Less": true, "Swap": true, "Push": true, "Pop": true,
	"Read": true, "Write": true, "Close": true, "ReadAt": true, "WriteAt": true, "Seek": true, "WriteString": true,
	"ServeHTTP": true, "RoundTrip": true, "Timeout": true, "Temporary": true, "Set": true,
}

var generatedRe = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)
var linknameRe = regexp.MustCompile(`^//go:linkname\s+(\S+)`)
var tagRe = regexp.MustCompile("(json|yaml|xml|toml|mapstructure|db|gorm|bson|msgpack|protobuf):\"([^\"]*)\"")

type analyzer struct {
	root    string
	modPath string
	patt    []string
	verbose bool
	log     io.Writer
}

func (a *analyzer) relPkg(p string) string {
	if p == a.modPath {
		return "."
	}
	return strings.TrimPrefix(p, a.modPath+"/")
}

func (a *analyzer) isInternal(p string) bool {
	r := a.relPkg(p)
	return strings.HasPrefix(r, "internal/") || strings.Contains(r, "/internal/")
}

func (a *analyzer) inModule(p string) bool {
	return p == a.modPath || strings.HasPrefix(p, a.modPath+"/")
}

func (a *analyzer) relFile(f string) string {
	if r, err := filepath.Rel(a.root, f); err == nil {
		return r
	}
	return f
}

func variantOf(id string) (base string, bracket string) {
	if i := strings.Index(id, " ["); i >= 0 {
		return id[:i], id[i+2 : len(id)-1]
	}
	return id, ""
}

func (a *analyzer) run(cfg config) (*result, error) {
	env := append(os.Environ(), "GOOS="+cfg.goos, "CGO_ENABLED=0")
	pc := &packages.Config{
		Dir:   a.root,
		Env:   env,
		Tests: true,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports |
			packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
	}
	if cfg.tags != "" {
		pc.BuildFlags = []string{"-tags=" + cfg.tags}
	}
	pkgs, err := packages.Load(pc, a.patt...)
	if err != nil {
		return nil, err
	}
	res := &result{
		decls: map[string]*decl{}, uses: map[string]*usage{}, exposed: map[string]string{},
		implBy: map[string][]string{}, satisfies: map[string][]string{}, inherit: map[string][]string{},
		linkname: map[string]struct{}{},
	}
	fset := (*token.FileSet)(nil)
	for _, p := range pkgs {
		if p.Fset != nil {
			fset = p.Fset
			break
		}
	}
	if fset == nil {
		return nil, fmt.Errorf("no packages loaded")
	}

	// Collect all packages (transitively) keyed by ID; classify module ones.
	all := map[string]*packages.Package{}
	packages.Visit(pkgs, nil, func(p *packages.Package) { all[p.ID] = p })

	var plain []*packages.Package // plain variants of module packages
	hasSelfTest := map[string]bool{}
	for _, p := range all {
		base, br := variantOf(p.ID)
		if br != "" && br == base+".test" {
			hasSelfTest[base] = true
		}
	}
	var toScan []*packages.Package
	for _, p := range all {
		if strings.HasSuffix(p.PkgPath, ".test") {
			continue // synthetic test main
		}
		// Errors count for every loaded package, not only this module's: a
		// broken dependency means the analysis below is not trustworthy.
		for _, e := range p.Errors {
			res.loadErrs = append(res.loadErrs, fmt.Sprintf("%s: %s", p.ID, e.Msg))
		}
		if !a.inModule(p.PkgPath) {
			continue
		}
		base, br := variantOf(p.ID)
		switch {
		case br == "":
			plain = append(plain, p)
			if !hasSelfTest[base] {
				toScan = append(toScan, p)
			}
		case br == base+".test": // package + in-package tests
			toScan = append(toScan, p)
		case strings.HasSuffix(p.PkgPath, "_test") && br == strings.TrimSuffix(p.PkgPath, "_test")+".test": // external test pkg
			toScan = append(toScan, p)
		default: // dependency rebuilt for another package's tests: redundant
		}
	}
	if len(plain) == 0 {
		return nil, fmt.Errorf("no module packages matched %v", a.patt)
	}
	sort.Slice(plain, func(i, j int) bool { return plain[i].ID < plain[j].ID })
	sort.Slice(toScan, func(i, j int) bool { return toScan[i].ID < toScan[j].ID })

	// ---- declarations (plain variants of internal packages)
	byPos := map[posKey]*decl{}
	pk := func(p token.Pos) posKey {
		ps := fset.Position(p)
		return posKey{ps.Filename, ps.Offset}
	}
	add := func(d *decl, id *ast.Ident, info *types.Info) {
		d.obj = info.Defs[id]
		d.pos = pk(id.Pos())
		ps := fset.Position(id.Pos())
		d.File, d.Line = a.relFile(ps.Filename), ps.Line
		d.Key = d.Pkg + "." + d.Name
		if _, dup := res.decls[d.Key]; dup {
			return
		}
		res.decls[d.Key] = d
		byPos[d.pos] = d
	}
	for _, p := range plain {
		if !a.isInternal(p.PkgPath) || p.TypesInfo == nil {
			continue
		}
		rp := a.relPkg(p.PkgPath)
		for _, f := range p.Syntax {
			fname := fset.Position(f.Pos()).Filename
			if strings.HasSuffix(fname, "_test.go") {
				continue
			}
			gen := ""
			for _, cg := range f.Comments {
				if cg.Pos() > f.Package {
					break
				}
				if generatedRe.MatchString(commentText(cg)) {
					gen = "generated file"
				}
			}
			a.collectLinkname(f, rp, res)
			for _, dc := range f.Decls {
				switch d := dc.(type) {
				case *ast.FuncDecl:
					if !d.Name.IsExported() {
						continue
					}
					if d.Recv == nil {
						add(&decl{Pkg: rp, Name: d.Name.Name, Kind: "func", Auto: gen}, d.Name, p.TypesInfo)
						continue
					}
					tn := recvName(d.Recv)
					if tn == "" || !ast.IsExported(tn) {
						continue
					}
					add(&decl{Pkg: rp, Name: tn + "." + d.Name.Name, Kind: "method", Auto: gen, ownerType: rp + "." + tn}, d.Name, p.TypesInfo)
				case *ast.GenDecl:
					for _, sp := range d.Specs {
						switch s := sp.(type) {
						case *ast.TypeSpec:
							if !s.Name.IsExported() {
								continue
							}
							kind := "type"
							if s.Assign.IsValid() {
								kind = "alias"
							}
							add(&decl{Pkg: rp, Name: s.Name.Name, Kind: kind, Auto: gen}, s.Name, p.TypesInfo)
							a.collectMembers(s.Type, rp, s.Name.Name, rp+"."+s.Name.Name, gen, p.TypesInfo, add)
						case *ast.ValueSpec:
							kind := "var"
							if d.Tok == token.CONST {
								kind = "const"
							}
							for _, n := range s.Names {
								if n.IsExported() {
									add(&decl{Pkg: rp, Name: n.Name, Kind: kind, Auto: gen}, n, p.TypesInfo)
								}
							}
						}
					}
				}
			}
		}
	}
	for k := range res.linkname {
		if d, ok := res.decls[k]; ok && d.Auto == "" {
			d.Auto = "go:linkname / export directive"
		}
	}

	// ---- uses (all scanned variants)
	mark := func(d *decl, refPkgPath string, xtest bool, test bool) {
		u := res.uses[d.Key]
		if u == nil {
			u = newUsage()
			res.uses[d.Key] = u
		}
		owner := d.Pkg
		ref := a.relPkg(refPkgPath)
		if xtest {
			ref = strings.TrimSuffix(ref, "_test")
		}
		switch {
		case ref == owner && xtest:
			u.xtest[ref] = struct{}{}
		case ref == owner && test:
			u.inTest = true
		case ref == owner:
			u.inProd = true
		case test:
			u.test[ref] = struct{}{}
		default:
			u.prod[ref] = struct{}{}
		}
	}
	for _, p := range toScan {
		if p.TypesInfo == nil {
			continue
		}
		xtest := p.Name != "" && strings.HasSuffix(p.Name, "_test") && strings.HasSuffix(p.PkgPath, "_test")
		fileTest := map[string]bool{}
		for _, f := range p.Syntax {
			fn := fset.Position(f.Pos()).Filename
			fileTest[fn] = strings.HasSuffix(fn, "_test.go")
		}
		for id, obj := range p.TypesInfo.Uses {
			if obj == nil || obj.Pkg() == nil || !a.inModule(obj.Pkg().Path()) {
				continue
			}
			d := byPos[pk(originPos(obj))]
			if d == nil {
				continue
			}
			ps := fset.Position(id.Pos())
			mark(d, p.PkgPath, xtest, fileTest[ps.Filename])
		}
		for _, f := range p.Syntax {
			fn := fset.Position(f.Pos()).Filename
			isTest := fileTest[fn]
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					// unkeyed struct literal references every field without idents
					if len(x.Elts) == 0 {
						return true
					}
					if _, kv := x.Elts[0].(*ast.KeyValueExpr); kv {
						return true
					}
					t := p.TypesInfo.TypeOf(x)
					if t == nil {
						return true
					}
					if ptr, ok := t.Underlying().(*types.Pointer); ok {
						t = ptr.Elem()
					}
					if st, ok := t.Underlying().(*types.Struct); ok {
						for i := 0; i < st.NumFields(); i++ {
							fv := st.Field(i)
							if fv.Pkg() == nil || !a.inModule(fv.Pkg().Path()) {
								continue
							}
							if d := byPos[pk(originPos(fv))]; d != nil {
								mark(d, p.PkgPath, xtest, isTest)
							}
						}
					}
				case *ast.CallExpr:
					if isTest {
						return true
					}
					a.reflectCall(x, p, byPos, pk, res)
				}
				return true
			})
		}
	}

	// ---- interface relations (plain variants only; one consistent type universe)
	a.interfaceRelations(all, plain, res)
	return res, nil
}

func commentText(cg *ast.CommentGroup) string {
	var b strings.Builder
	for _, c := range cg.List {
		b.WriteString(c.Text)
		b.WriteByte('\n')
	}
	return b.String()
}

func originPos(o types.Object) token.Pos {
	switch v := o.(type) {
	case *types.Func:
		return v.Origin().Pos()
	case *types.Var:
		return v.Origin().Pos()
	}
	return o.Pos()
}

func recvName(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	t := fl.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
		case *ast.ParenExpr:
			t = x.X
		case *ast.IndexExpr:
			t = x.X
		case *ast.IndexListExpr:
			t = x.X
		case *ast.Ident:
			return x.Name
		default:
			return ""
		}
	}
}

func (a *analyzer) collectLinkname(f *ast.File, rp string, res *result) {
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if m := linknameRe.FindStringSubmatch(c.Text); m != nil {
				res.linkname[rp+"."+m[1]] = struct{}{}
			}
		}
	}
}

// collectMembers records exported struct fields and interface methods.
func (a *analyzer) collectMembers(t ast.Expr, rp, qual, ownerKey, gen string, info *types.Info, add func(*decl, *ast.Ident, *types.Info)) {
	switch x := t.(type) {
	case *ast.StructType:
		for _, f := range x.Fields.List {
			auto := gen
			if f.Tag != nil {
				tag := f.Tag.Value
				for _, m := range tagRe.FindAllStringSubmatch(tag, -1) {
					if m[2] != "-" && auto == "" {
						auto = "serialization tag (" + m[1] + ")"
					}
				}
			}
			if len(f.Names) == 0 { // embedded
				n := embeddedIdent(f.Type)
				if n != nil && n.IsExported() {
					if auto == "" {
						auto = "embedded field (promotion)"
					}
					add(&decl{Pkg: rp, Name: qual + "." + n.Name, Kind: "field", Auto: auto, ownerType: ownerKey, embedded: true}, n, info)
				}
				continue
			}
			for _, n := range f.Names {
				if n.IsExported() {
					add(&decl{Pkg: rp, Name: qual + "." + n.Name, Kind: "field", Auto: auto, ownerType: ownerKey}, n, info)
				}
				a.collectMembers(f.Type, rp, qual+"."+n.Name, ownerKey, gen, info, add)
			}
		}
	case *ast.InterfaceType:
		for _, f := range x.Methods.List {
			for _, n := range f.Names { // embedded ifaces have no Names
				if n.IsExported() {
					add(&decl{Pkg: rp, Name: qual + "." + n.Name, Kind: "ifacemethod", Auto: gen, ownerType: ownerKey}, n, info)
				}
			}
		}
	}
}

func embeddedIdent(e ast.Expr) *ast.Ident {
	for {
		switch x := e.(type) {
		case *ast.StarExpr:
			e = x.X
		case *ast.SelectorExpr:
			return x.Sel
		case *ast.IndexExpr:
			e = x.X
		case *ast.IndexListExpr:
			e = x.X
		case *ast.Ident:
			return x
		default:
			return nil
		}
	}
}

// reflectCall marks module struct types reachable from arguments of calls into
// encoding/*, reflect, template packages as exposed (all exported fields needed).
func (a *analyzer) reflectCall(c *ast.CallExpr, p *packages.Package, byPos map[posKey]*decl, pk func(token.Pos) posKey, res *result) {
	var id *ast.Ident
	switch f := c.Fun.(type) {
	case *ast.Ident:
		id = f
	case *ast.SelectorExpr:
		id = f.Sel
	default:
		return
	}
	obj := p.TypesInfo.Uses[id]
	if obj == nil || obj.Pkg() == nil || !serialPkgs[obj.Pkg().Path()] {
		return
	}
	pos := p.Fset.Position(c.Pos())
	why := fmt.Sprintf("passed to %s.%s at %s:%d", obj.Pkg().Path(), obj.Name(), a.relFile(pos.Filename), pos.Line)
	seen := map[types.Type]bool{}
	var walk func(t types.Type)
	walk = func(t types.Type) {
		if t == nil || seen[t] {
			return
		}
		seen[t] = true
		switch x := t.(type) {
		case *types.Alias:
			walk(types.Unalias(x))
		case *types.Pointer:
			walk(x.Elem())
		case *types.Slice:
			walk(x.Elem())
		case *types.Array:
			walk(x.Elem())
		case *types.Map:
			walk(x.Key())
			walk(x.Elem())
		case *types.Named:
			if o := x.Obj(); o.Pkg() != nil && a.inModule(o.Pkg().Path()) {
				if d := byPos[pk(o.Pos())]; d != nil {
					if _, ok := res.exposed[d.Key]; !ok {
						res.exposed[d.Key] = why
					}
				}
				walk(x.Underlying())
			}
		case *types.Struct:
			for i := 0; i < x.NumFields(); i++ {
				walk(x.Field(i).Type())
			}
		}
	}
	for _, arg := range c.Args {
		walk(p.TypesInfo.TypeOf(arg))
	}
}

// interfaceRelations computes, using plain package variants only:
//   - impl method -> external contract (stdlib / other-package interface) => auto exempt
//   - impl method -> same-package interface method => inherits that method's uses
//   - interface method -> implementers in other packages => auto exempt
func (a *analyzer) interfaceRelations(all map[string]*packages.Package, plain []*packages.Package, res *result) {
	type ifc struct {
		it    *types.Interface
		pkg   string // import path ("" universe)
		label string
		key   string // decl key prefix for module-internal exported iface ("" otherwise)
	}
	byMethod := map[string][]*ifc{}
	addIface := func(it *types.Interface, pkg, label, key string) {
		if it.NumMethods() == 0 {
			return
		}
		x := &ifc{it, pkg, label, key}
		for i := 0; i < it.NumMethods(); i++ {
			m := it.Method(i)
			if m.Exported() {
				byMethod[m.Name()] = append(byMethod[m.Name()], x)
			}
		}
	}
	if o := types.Universe.Lookup("error"); o != nil {
		addIface(o.Type().Underlying().(*types.Interface), "", "error", "")
	}
	ids := make([]string, 0, len(all))
	for id := range all {
		if _, br := variantOf(id); br == "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var modTypes []*types.TypeName
	for _, id := range ids {
		p := all[id]
		if p.Types == nil || strings.HasSuffix(p.PkgPath, ".test") {
			continue
		}
		sc := p.Types.Scope()
		for _, n := range sc.Names() {
			tn, ok := sc.Lookup(n).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || named.TypeParams().Len() > 0 {
				continue
			}
			if it, ok := named.Underlying().(*types.Interface); ok {
				if !tn.Exported() {
					continue // unexported interfaces cannot be a contract for other packages
				}
				key := ""
				if a.inModule(p.PkgPath) && a.isInternal(p.PkgPath) && tn.Exported() {
					key = a.relPkg(p.PkgPath) + "." + tn.Name()
				}
				addIface(it, p.PkgPath, p.PkgPath+"."+tn.Name(), key)
			} else if a.inModule(p.PkgPath) {
				modTypes = append(modTypes, tn)
			}
		}
	}
	// implementers: candidate decl types
	for _, tn := range modTypes {
		pp := tn.Pkg().Path()
		if !a.isInternal(pp) || !tn.Exported() {
			// still counts as an implementer of module ifaces (below)
		}
		T := tn.Type()
		ptr := types.NewPointer(T)
		ms := types.NewMethodSet(ptr)
		for i := 0; i < ms.Len(); i++ {
			m, ok := ms.At(i).Obj().(*types.Func)
			if !ok || !m.Exported() {
				continue
			}
			mkey := ""
			if a.isInternal(pp) && tn.Exported() && m.Pkg() != nil && m.Pkg().Path() == pp {
				mkey = a.relPkg(pp) + "." + tn.Name() + "." + m.Name()
			}
			for _, x := range byMethod[m.Name()] {
				if !(types.Implements(T, x.it) || types.Implements(ptr, x.it)) {
					continue
				}
				implLabel := a.relPkg(pp) + "." + tn.Name()
				if x.pkg != pp {
					if mkey != "" {
						res.satisfies[mkey] = appendUniq(res.satisfies[mkey], x.label)
					}
					if x.key != "" {
						ik := x.key + "." + m.Name()
						res.implBy[ik] = appendUniq(res.implBy[ik], implLabel)
					}
				} else if x.key != "" && mkey != "" {
					res.inherit[mkey] = appendUniq(res.inherit[mkey], x.key+"."+m.Name())
				}
			}
		}
	}
}

func appendUniq(s []string, v string) []string {
	for _, e := range s {
		if e == v {
			return s
		}
	}
	return append(s, v)
}

// ---------------------------------------------------------------- merging

type merged struct {
	decls     map[string]*decl
	uses      map[string]*usage
	exposed   map[string]string
	implBy    map[string][]string
	satisfies map[string][]string
	inherit   map[string][]string
	cfgSeen   map[string][]string // decl key -> configs where declared
}

func mergeAll(rs map[config]*result, cfgs []config) *merged {
	m := &merged{decls: map[string]*decl{}, uses: map[string]*usage{}, exposed: map[string]string{},
		implBy: map[string][]string{}, satisfies: map[string][]string{}, inherit: map[string][]string{},
		cfgSeen: map[string][]string{}}
	for _, c := range cfgs {
		r := rs[c]
		for k, d := range r.decls {
			if _, ok := m.decls[k]; !ok {
				m.decls[k] = d
			}
			if d.Auto != "" && m.decls[k].Auto == "" {
				m.decls[k].Auto = d.Auto
			}
			m.cfgSeen[k] = append(m.cfgSeen[k], c.String())
		}
		for k, u := range r.uses {
			if m.uses[k] == nil {
				m.uses[k] = newUsage()
			}
			m.uses[k].merge(u)
		}
		for k, v := range r.exposed {
			if _, ok := m.exposed[k]; !ok {
				m.exposed[k] = v
			}
		}
		for k, v := range r.implBy {
			for _, s := range v {
				m.implBy[k] = appendUniq(m.implBy[k], s)
			}
		}
		for k, v := range r.satisfies {
			for _, s := range v {
				m.satisfies[k] = appendUniq(m.satisfies[k], s)
			}
		}
		for k, v := range r.inherit {
			for _, s := range v {
				m.inherit[k] = appendUniq(m.inherit[k], s)
			}
		}
	}
	return m
}

// ---------------------------------------------------------------- exemptions

type exemptions struct {
	exact   map[string]string
	prefix  map[string]string // key without trailing ".*"
	touched map[string]bool
}

func loadExemptions(path string) (*exemptions, error) {
	e := &exemptions{exact: map[string]string{}, prefix: map[string]string{}, touched: map[string]bool{}}
	if path == "" {
		return e, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for i, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		name, reason := t, ""
		if j := strings.Index(t, "#"); j >= 0 {
			name, reason = strings.TrimSpace(t[:j]), strings.TrimSpace(t[j+1:])
		}
		if reason == "" {
			return nil, fmt.Errorf("%s:%d: exemption %q has no reason comment (format: `pkg.Name  # why`)", path, i+1, name)
		}
		if strings.HasSuffix(name, ".*") {
			e.prefix[strings.TrimSuffix(name, ".*")] = reason
		} else {
			e.exact[name] = reason
		}
	}
	return e, nil
}

func (e *exemptions) match(key string) (string, bool) {
	if r, ok := e.exact[key]; ok {
		e.touched[key] = true
		return r, true
	}
	for p, r := range e.prefix {
		if strings.HasPrefix(key, p+".") {
			e.touched[p+".*"] = true
			return r, true
		}
	}
	return "", false
}

func (e *exemptions) stale() []string {
	var s []string
	for k := range e.exact {
		if !e.touched[k] {
			s = append(s, k)
		}
	}
	for k := range e.prefix {
		if !e.touched[k+".*"] {
			s = append(s, k+".*")
		}
	}
	sort.Strings(s)
	return s
}

// ---------------------------------------------------------------- report

type item struct {
	Name   string   `json:"name"`
	Kind   string   `json:"kind"`
	File   string   `json:"file"`
	Line   int      `json:"line"`
	ProdBy []string `json:"production_referrers,omitempty"`
	TestBy []string `json:"test_referrers,omitempty"`
	InPkg  string   `json:"in_package,omitempty"` // "prod", "test", "prod+test", "xtest", "none"
	Reason string   `json:"exempt_reason,omitempty"`
	Would  string   `json:"would_be,omitempty"` // category an exempt item would fall in
}

type pkgReport struct {
	Pkg      string `json:"package"`
	Needed   []item `json:"needed_by_production_elsewhere"`
	TestOnly []item `json:"only_external_tests"`
	Nobody   []item `json:"nobody_outside_unexport_candidate"`
	Exempt   []item `json:"exempt"`
}

type totals struct {
	Declared int `json:"declared"`
	Needed   int `json:"needed_by_production_elsewhere"`
	TestOnly int `json:"only_external_tests"`
	Nobody   int `json:"nobody_outside_unexport_candidate"`
	Exempt   int `json:"exempt"`
}

type report struct {
	Module   string                    `json:"module"`
	Configs  []string                  `json:"configs"`
	Packages []pkgReport               `json:"packages"`
	Totals   totals                    `json:"totals"`
	Kinds    map[string]map[string]int `json:"by_kind"` // category -> kind -> n
	Stale    []string                  `json:"stale_exemptions,omitempty"`
	Errors   []string                  `json:"load_errors,omitempty"`
}

func sortedKeys(m map[string]struct{}) []string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

func build(modPath string, cfgs []config, m *merged, ex *exemptions) *report {
	rep := &report{Module: modPath, Kinds: map[string]map[string]int{}}
	for _, c := range cfgs {
		rep.Configs = append(rep.Configs, c.String())
	}
	// effective usage incl. inheritance from same-package interface methods
	eff := func(key string) *usage {
		u := newUsage()
		if x := m.uses[key]; x != nil {
			u.merge(x)
		}
		for _, ik := range m.inherit[key] {
			if x := m.uses[ik]; x != nil {
				u.merge(x)
			}
		}
		return u
	}
	pk := map[string]*pkgReport{}
	keys := make([]string, 0, len(m.decls))
	for k := range m.decls {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	count := func(cat, kind string) {
		if rep.Kinds[cat] == nil {
			rep.Kinds[cat] = map[string]int{}
		}
		rep.Kinds[cat][kind]++
	}
	for _, k := range keys {
		d := m.decls[k]
		pr := pk[d.Pkg]
		if pr == nil {
			pr = &pkgReport{Pkg: d.Pkg}
			pk[d.Pkg] = pr
		}
		u := eff(k)
		it := item{Name: d.Name, Kind: d.Kind, File: d.File, Line: d.Line}
		it.ProdBy = sortedKeys(u.prod)
		it.TestBy = sortedKeys(u.test)
		ip := []string{}
		if u.inProd {
			ip = append(ip, "prod")
		}
		if u.inTest {
			ip = append(ip, "test")
		}
		if len(u.xtest) > 0 {
			ip = append(ip, "xtest")
		}
		it.InPkg = strings.Join(ip, "+")
		if it.InPkg == "" {
			it.InPkg = "none"
		}
		cat := "nobody"
		switch {
		case len(u.prod) > 0:
			cat = "needed"
		case len(u.test) > 0:
			cat = "testonly"
		}
		reason := ""
		if r, ok := ex.match(k); ok {
			reason = "exemption file: " + r
		} else if d.Auto != "" {
			reason = "auto: " + d.Auto
		} else if r, ok := m.exposed[d.ownerType]; ok && d.Kind == "field" {
			reason = "auto: reflect/serialization exposure of " + d.ownerType + " (" + r + ")"
		} else if v := m.satisfies[k]; len(v) > 0 && d.Kind == "method" {
			reason = "auto: satisfies " + strings.Join(trim(v, 3), ", ")
		} else if d.Kind == "method" && wellKnownMethods[d.Name[strings.LastIndex(d.Name, ".")+1:]] {
			reason = "auto: well-known protocol method name (" + d.Name[strings.LastIndex(d.Name, ".")+1:] + ")"
		} else if v := m.implBy[k]; len(v) > 0 && d.Kind == "ifacemethod" {
			reason = "auto: implemented in other package by " + strings.Join(trim(v, 3), ", ")
		}
		if reason != "" && cat != "needed" {
			it.Reason, it.Would = reason, cat
			pr.Exempt = append(pr.Exempt, it)
			rep.Totals.Exempt++
			count("exempt", d.Kind)
		} else {
			switch cat {
			case "needed":
				pr.Needed = append(pr.Needed, it)
				rep.Totals.Needed++
			case "testonly":
				pr.TestOnly = append(pr.TestOnly, it)
				rep.Totals.TestOnly++
			default:
				pr.Nobody = append(pr.Nobody, it)
				rep.Totals.Nobody++
			}
			count(cat, d.Kind)
		}
		rep.Totals.Declared++
	}
	names := make([]string, 0, len(pk))
	for n := range pk {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		rep.Packages = append(rep.Packages, *pk[n])
	}
	rep.Stale = ex.stale()
	return rep
}

func trim(s []string, n int) []string {
	s = append([]string(nil), s...)
	sort.Strings(s)
	if len(s) > n {
		s = append(s[:n], fmt.Sprintf("+%d more", len(s)-n))
	}
	return s
}

func writeText(w io.Writer, r *report, summary bool, strict bool) {
	fmt.Fprintf(w, "exportcheck: module %s\nconfigurations: %s\n\n", r.Module, strings.Join(r.Configs, ", "))
	fmt.Fprintf(w, "%-34s %8s %8s %9s %8s %7s\n", "package", "declared", "needed", "test-only", "nobody", "exempt")
	for _, p := range r.Packages {
		fmt.Fprintf(w, "%-34s %8d %8d %9d %8d %7d\n", p.Pkg, len(p.Needed)+len(p.TestOnly)+len(p.Nobody)+len(p.Exempt),
			len(p.Needed), len(p.TestOnly), len(p.Nobody), len(p.Exempt))
	}
	t := r.Totals
	fmt.Fprintf(w, "%-34s %8d %8d %9d %8d %7d\n\n", "TOTAL", t.Declared, t.Needed, t.TestOnly, t.Nobody, t.Exempt)
	cats := []string{"needed", "testonly", "nobody", "exempt"}
	for _, c := range cats {
		if len(r.Kinds[c]) == 0 {
			continue
		}
		ks := make([]string, 0)
		for k := range r.Kinds[c] {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var parts []string
		for _, k := range ks {
			parts = append(parts, fmt.Sprintf("%s=%d", k, r.Kinds[c][k]))
		}
		fmt.Fprintf(w, "by kind, %-9s %s\n", c+":", strings.Join(parts, " "))
	}
	fmt.Fprintln(w)
	if !summary {
		for _, p := range r.Packages {
			fmt.Fprintf(w, "== %s\n", p.Pkg)
			sect := func(title string, items []item, detail func(item) string) {
				if len(items) == 0 {
					return
				}
				fmt.Fprintf(w, "  %s (%d)\n", title, len(items))
				for _, it := range items {
					fmt.Fprintf(w, "    %-11s %s%s\n", it.Kind, it.Name, detail(it))
				}
			}
			sect("needed by production elsewhere", p.Needed, func(i item) string { return "  <- " + strings.Join(trim(i.ProdBy, 4), ",") })
			sect("only external tests (relocation candidates)", p.TestOnly, func(i item) string { return "  <- tests in " + strings.Join(trim(i.TestBy, 4), ",") })
			sect("nobody outside (un-export candidates)", p.Nobody, func(i item) string { return "  [in-pkg: " + i.InPkg + "]  " + i.File + ":" + fmt.Sprint(i.Line) })
			sect("exempt", p.Exempt, func(i item) string { return "  [" + i.Would + "] " + i.Reason })
		}
	}
	if len(r.Stale) > 0 {
		fmt.Fprintf(w, "\nstale exemptions (match no declaration):\n")
		for _, s := range r.Stale {
			fmt.Fprintf(w, "  %s\n", s)
		}
	}
	if strict && r.Totals.Nobody > 0 {
		fmt.Fprintf(w, "\nFAIL: %d exported name(s) in internal packages are not referenced by any other package's production code.\n", r.Totals.Nobody)
		fmt.Fprint(w, `
How to fix (per name, see the "nobody outside" lists above):
  1. Un-export it:   gopls rename -w <file>:<line>:<col> <lowerCamelName>
                     (or: gofmt -r 'Old -> old' -w <pkg dir>, then fix tests that referenced it)
  2. Or, if it must stay exported (interface contract, reflection, encoding), add a line to the
     exemption file with a mandatory reason:
         internal/<pkg>.<Type>.<Name>   # why this has to be exported
     (a trailing ".*" exempts every member: internal/<pkg>.<Type>.*)
`)
	}
}

// ---------------------------------------------------------------- main

func main() {
	var (
		dir       = flag.String("dir", ".", "module root directory")
		pattern   = flag.String("pkgs", "./...", "package pattern(s), comma separated")
		exFile    = flag.String("exempt", "", "exemption file (lines: `internal/pkg.Name  # reason`)")
		jsonOut   = flag.String("json", "", "write JSON report to this file ('-' for stdout)")
		textOut   = flag.String("text", "-", "write text report to this file ('-' for stdout, '' to disable)")
		strict    = flag.Bool("strict", false, "exit 1 if any non-exempt un-export candidate exists")
		strictTst = flag.Bool("strict-tests", false, "with -strict (implied), also fail on names used only by other packages' tests")
		summary   = flag.Bool("summary", false, "text report: totals only, no per-name lists")
		goosList  = flag.String("goos", "linux,darwin", "comma separated GOOS values")
		tagList   = flag.String("tagsets", ",integration", "comma separated build-tag sets (empty entry = default); use '+' inside a set to join tags")
		verbose   = flag.Bool("v", false, "print timings to stderr")
	)
	flag.Parse()
	root, err := filepath.Abs(*dir)
	if err != nil {
		fatal(err)
	}
	modPath, err := modulePath(root)
	if err != nil {
		fatal(err)
	}
	ex, err := loadExemptions(*exFile)
	if err != nil {
		fatal(err)
	}
	var cfgs []config
	for _, g := range strings.Split(*goosList, ",") {
		for _, t := range strings.Split(*tagList, ",") {
			cfgs = append(cfgs, config{strings.TrimSpace(g), strings.ReplaceAll(strings.TrimSpace(t), "+", ",")})
		}
	}
	a := &analyzer{root: root, modPath: modPath, patt: strings.Split(*pattern, ","), verbose: *verbose}
	rs := map[config]*result{}
	start := time.Now()
	var loadErrs []string
	for _, c := range cfgs {
		t0 := time.Now()
		r, err := a.run(c)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", c, err))
		}
		rs[c] = r
		for _, e := range r.loadErrs {
			loadErrs = append(loadErrs, c.String()+": "+e)
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "exportcheck: %-22s %6.1fs  %d decls\n", c, time.Since(t0).Seconds(), len(r.decls))
		}
	}
	m := mergeAll(rs, cfgs)
	rep := build(modPath, cfgs, m, ex)
	sort.Strings(loadErrs)
	rep.Errors = dedupe(loadErrs)
	if *verbose {
		fmt.Fprintf(os.Stderr, "exportcheck: total %.1fs\n", time.Since(start).Seconds())
	}
	if len(rep.Errors) > 0 {
		fmt.Fprintf(os.Stderr, "exportcheck: warning: %d package load error(s); first: %s\n", len(rep.Errors), rep.Errors[0])
	}
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		b = append(b, '\n')
		if *jsonOut == "-" {
			os.Stdout.Write(b)
		} else if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			fatal(err)
		}
	}
	failed := rep.Totals.Nobody > 0 || (*strictTst && rep.Totals.TestOnly > 0)
	if *textOut != "" && !(*jsonOut == "-" && *textOut == "-") {
		w := io.Writer(os.Stdout)
		if *textOut != "-" {
			f, err := os.Create(*textOut)
			if err != nil {
				fatal(err)
			}
			defer f.Close()
			w = f
		}
		writeText(w, rep, *summary, *strict || *strictTst)
		if *strictTst && rep.Totals.TestOnly > 0 {
			fmt.Fprintf(w, "\nFAIL (-strict-tests): %d name(s) are used only by other packages' tests: move the test into the owning package or relocate the code.\n", rep.Totals.TestOnly)
		}
	}
	if (*strict || *strictTst) && failed {
		os.Exit(1)
	}
	// A package that failed to load was not analysed, so a clean result is not
	// trustworthy: fail regardless of strictness.
	if len(rep.Errors) > 0 {
		os.Exit(1)
	}
}

func dedupe(s []string) []string {
	var out []string
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func modulePath(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, l := range strings.Split(string(b), "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(l, "module ")), `"`), nil
		}
	}
	return "", fmt.Errorf("no module line in go.mod")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "exportcheck:", err)
	os.Exit(2)
}
