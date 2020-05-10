package cl

import (
	"errors"
	"path"
	"reflect"
	"syscall"

	"github.com/qiniu/qlang/ast"
	"github.com/qiniu/qlang/ast/astutil"
	"github.com/qiniu/qlang/exec"
	"github.com/qiniu/qlang/token"
	"github.com/qiniu/x/log"
)

var (
	// ErrNotFound error.
	ErrNotFound = syscall.ENOENT

	// ErrMainFuncNotFound error.
	ErrMainFuncNotFound = errors.New("main function not found")

	// ErrSymbolNotVariable error.
	ErrSymbolNotVariable = errors.New("symbol exists but not a variable")

	// ErrSymbolNotFunc error.
	ErrSymbolNotFunc = errors.New("symbol exists but not a func")

	// ErrSymbolNotType error.
	ErrSymbolNotType = errors.New("symbol exists but not a type")
)

// -----------------------------------------------------------------------------

type pkgCtx struct {
	infer   exec.Stack
	builtin *exec.GoPackage
	out     *exec.Builder
	usedfns []*funcDecl
}

func newPkgCtx(out *exec.Builder) *pkgCtx {
	p := &pkgCtx{builtin: exec.FindGoPackage(""), out: out}
	p.infer.Init()
	return p
}

func (p *pkgCtx) use(f *funcDecl) {
	if f.used {
		return
	}
	p.usedfns = append(p.usedfns, f)
	f.used = true
}

func (p *pkgCtx) resolveFuncs() {
	for {
		n := len(p.usedfns)
		if n == 0 {
			break
		}
		f := p.usedfns[n-1]
		p.usedfns = p.usedfns[:n-1]
		f.compile()
	}
}

type fileCtx struct {
	*blockCtx // it's global blockCtx
	imports   map[string]string
}

func newFileCtx(block *blockCtx) *fileCtx {
	return &fileCtx{blockCtx: block, imports: make(map[string]string)}
}

// -----------------------------------------------------------------------------

// - varName => *exec.Var
// - pkgName => pkgPath
// - funcName => *funcDecl
// - typeName => *typeDecl
//
type iSymbol = interface{}

type blockCtx struct {
	*pkgCtx
	file   *fileCtx
	parent *blockCtx

	vlist []*exec.Var
	syms  map[string]iSymbol
}

func newBlockCtx(file *fileCtx) *blockCtx {
	return &blockCtx{
		pkgCtx: file.pkgCtx,
		file:   file,
		parent: file.blockCtx,
		syms:   make(map[string]iSymbol),
	}
}

func newGblBlockCtx(pkg *pkgCtx, parent *blockCtx) *blockCtx {
	return &blockCtx{
		pkgCtx: pkg,
		parent: parent,
		syms:   make(map[string]iSymbol),
	}
}

func (p *blockCtx) exists(name string) (ok bool) {
	if _, ok = p.syms[name]; ok {
		return
	}
	if p.parent == nil { // it's global blockCtx
		_, ok = p.file.imports[name]
	}
	return
}

func (p *blockCtx) find(name string) (sym interface{}, ok bool) {
	ctx := p
	for ; p != nil; p = p.parent {
		if sym, ok = p.syms[name]; ok {
			return
		}
	}
	sym, ok = ctx.file.imports[name]
	return
}

func (p *blockCtx) findType(name string) (decl *typeDecl, err error) {
	v, ok := p.find(name)
	if !ok {
		return nil, ErrNotFound
	}
	if decl, ok = v.(*typeDecl); ok {
		return
	}
	return nil, ErrSymbolNotType
}

func (p *blockCtx) findFunc(name string) (addr *funcDecl, err error) {
	v, ok := p.find(name)
	if !ok {
		return nil, ErrNotFound
	}
	if addr, ok = v.(*funcDecl); ok {
		return
	}
	return nil, ErrSymbolNotFunc
}

func (p *blockCtx) findVar(name string) (addr *exec.Var, err error) {
	v, ok := p.find(name)
	if !ok {
		return nil, ErrNotFound
	}
	if addr, ok = v.(*exec.Var); ok {
		return
	}
	return nil, ErrSymbolNotVariable
}

func (p *blockCtx) insertVar(name string, typ reflect.Type) *exec.Var {
	if p.exists(name) {
		log.Panicln("insertVar failed: symbol exists -", name)
	}
	idx := uint32(len(p.vlist))
	v := exec.NewVar(typ, name)
	v.SetAddr(p.out.NestDepth, idx)
	p.syms[name] = v
	p.vlist = append(p.vlist, v)
	return v
}

func (p *blockCtx) insertFunc(name string, fun *funcDecl) {
	if p.exists(name) {
		log.Panicln("insertFunc failed: symbol exists -", name)
	}
	p.syms[name] = fun
}

func (p *blockCtx) insertMethod(typeName, methodName string, method *methodDecl) {
	if p.parent != nil {
		log.Panicln("insertMethod failed: unexpected - non global method declaration?")
	}
	typ, err := p.findType(typeName)
	if err == ErrNotFound {
		typ = new(typeDecl)
		p.syms[typeName] = typ
	} else if err != nil {
		log.Panicln("insertMethod failed:", err)
	} else if typ.Alias {
		log.Panicln("insertMethod failed: alias?")
	}
	if typ.Methods == nil {
		typ.Methods = map[string]*methodDecl{methodName: method}
	} else {
		if _, ok := typ.Methods[methodName]; ok {
			log.Panicln("insertMethod failed: method exists -", typeName, methodName)
		}
		typ.Methods[methodName] = method
	}
}

// -----------------------------------------------------------------------------

// A Package represents a qlang package.
type Package struct {
	vlist []*exec.Var
	syms  map[string]iSymbol
}

// NewPackage creates a qlang package instance.
func NewPackage(out *exec.Builder, pkg *ast.Package) (p *Package, err error) {
	p = &Package{}
	ctxPkg := newPkgCtx(out)
	ctx := newGblBlockCtx(ctxPkg, nil)
	for _, f := range pkg.Files {
		loadFile(ctx, f)
	}
	if pkg.Name == "main" {
		entry, err := ctx.findFunc("main")
		if err != nil {
			if err == ErrNotFound {
				err = ErrMainFuncNotFound
			}
			return p, err
		}
		ctx.file = entry.file
		compileBlockStmt(ctx, entry.body)
		ctxPkg.resolveFuncs()
	}
	p.vlist = ctx.vlist
	p.syms = ctx.syms
	return
}

// GetGlobalVars returns the global variable list.
func (p *Package) GetGlobalVars() []*exec.Var {
	return p.vlist
}

func loadFile(block *blockCtx, f *ast.File) {
	ctx := newFileCtx(block)
	block.file = ctx
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			loadFunc(ctx, d)
		case *ast.GenDecl:
			switch d.Tok {
			case token.IMPORT:
				loadImports(ctx, d)
			case token.TYPE:
				loadTypes(d)
			case token.CONST:
				loadConsts(d)
			case token.VAR:
				loadVars(d)
			default:
				log.Panicln("tok:", d.Tok, "spec:", reflect.TypeOf(d.Specs).Elem())
			}
		default:
			log.Panicln("gopkg.Package.load: unknown decl -", reflect.TypeOf(decl))
		}
	}
}

func loadImports(ctx *fileCtx, d *ast.GenDecl) {
	for _, item := range d.Specs {
		loadImport(ctx, item.(*ast.ImportSpec))
	}
}

func loadImport(ctx *fileCtx, spec *ast.ImportSpec) {
	var pkgPath = astutil.ToString(spec.Path)
	var name string
	if spec.Name != nil {
		name = spec.Name.Name
		switch name {
		case "_", ".":
			panic("not impl")
		}
	} else {
		name = path.Base(pkgPath)
	}
	ctx.imports[name] = pkgPath
}

func loadTypes(d *ast.GenDecl) {
	for _, item := range d.Specs {
		loadType(item.(*ast.TypeSpec))
	}
}

func loadType(spec *ast.TypeSpec) {
}

func loadConsts(d *ast.GenDecl) {
}

func loadVars(d *ast.GenDecl) {
	for _, item := range d.Specs {
		loadVar(item.(*ast.ValueSpec))
	}
}

func loadVar(spec *ast.ValueSpec) {
}

func loadFunc(ctx *fileCtx, d *ast.FuncDecl) {
	var name = d.Name.Name
	if d.Recv != nil {
		recv := astutil.ToRecv(d.Recv)
		ctx.insertMethod(recv.Type, name, &methodDecl{
			recv:    recv.Name,
			pointer: recv.Pointer,
			typ:     d.Type,
			body:    d.Body,
			file:    ctx,
		})
	} else if name == "init" {
		log.Panicln("loadFunc TODO: init")
	} else {
		ctx.insertFunc(name, newFuncDecl(name, d.Type, d.Body, ctx))
	}
}

// -----------------------------------------------------------------------------
