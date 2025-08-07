package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"os"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

func fatal(context string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", context, err)
	os.Exit(1)
}

func main() {
	fset := &token.FileSet{}
	cfg := &packages.Config{
		Mode: packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedName | packages.NeedCompiledGoFiles | packages.NeedTypes,
		Fset: fset,
	}
	pkgs, err := packages.Load(cfg, "github.com/go-delve/delve/service/rpc2")
	if err != nil {
		fatal("packages.Load", err)
	}
	var clientAst *ast.File
	var serverMethods map[string]*types.Func
	var info *types.Info
	packages.Visit(pkgs, func(pkg *packages.Package) bool {
		if pkg.PkgPath != "github.com/go-delve/delve/service/rpc2" {
			return true
		}
		log.Printf("package found: %v", pkg.PkgPath)
		serverMethods = getMethods(pkg.Types, "RPCServer")
		info = pkg.TypesInfo
		for i := range pkg.Syntax {
			log.Printf("file %q", pkg.CompiledGoFiles[i])
			if strings.HasSuffix(pkg.CompiledGoFiles[i], string(os.PathSeparator)+"client.go") {
				clientAst = pkg.Syntax[i]
				break
			}
		}
		return true
	}, nil)

	errcount := 0

	for _, decl := range clientAst.Decls {
		fndecl := publicMethodOf(decl, "RPCClient")
		if fndecl == nil {
			continue
		}

		switch fndecl.Name.Name {
		case "Continue", "Rewind":
			// wrappers over continueDir
			continue
		case "SetReturnValuesLoadConfig", "Disconnect", "SetEventsFn":
			// support functions
			continue
		}

		if fndecl.Name.Name == "Continue" || fndecl.Name.Name == "Rewind" || fndecl.Name.Name == "DirectionCongruentContinue" {
			// using continueDir
			continue
		}

		callx := findCallCall(fndecl)

		if callx == nil {
			log.Printf("%s: could not find RPC call", fset.Position(fndecl.Pos()))
			errcount++
			continue
		}

		if len(callx.Args) != 3 {
			log.Printf("%s: wrong number of arguments for RPC call", fset.Position(callx.Pos()))
			errcount++
			continue
		}

		arg0, arg0islit := callx.Args[0].(*ast.BasicLit)
		arg1 := callx.Args[1]
		arg2 := callx.Args[2]
		if !arg0islit || arg0.Kind != token.STRING {
			continue
		}
		name, _ := strconv.Unquote(arg0.Value)
		serverMethod := serverMethods[name]
		if serverMethod == nil {
			log.Printf("%s: could not find RPC method %q", fset.Position(callx.Pos()), name)
			errcount++
			continue
		}

		params := serverMethod.Type().(*types.Signature).Params()

		if a, e := info.TypeOf(arg1), params.At(0).Type(); !types.AssignableTo(a, e) {
			log.Printf("%s: wrong type of first argument %s, expected %s", fset.Position(callx.Pos()), types.TypeString(a, qf), types.TypeString(e, qf))
			errcount++
			continue
		}

		if !strings.HasSuffix(params.At(1).Type().String(), "/service.RPCCallback") {
			if a, e := info.TypeOf(arg2), params.At(1).Type(); !types.AssignableTo(a, e) {
				log.Printf("%s: wrong type of second argument %s, expected %s", fset.Position(callx.Pos()), types.TypeString(a, qf), types.TypeString(e, qf))
				errcount++
				continue
			}
		}

		if clit, ok := arg1.(*ast.CompositeLit); ok {
			typ := params.At(0).Type()
			st := typ.Underlying().(*types.Struct)
			if len(clit.Elts) != st.NumFields() && types.TypeString(typ, qf) != "DebuggerCommand" {
				log.Printf("%s: wrong number of fields in first argument's literal %d, expected %d", fset.Position(callx.Pos()), len(clit.Elts), st.NumFields())
				errcount++
				continue
			}
		}
	}

	if errcount > 0 {
		fmt.Fprintf(os.Stderr, "previous errors\n")
		os.Exit(1)
	}
}

func getMethods(pkg *types.Package, typename string) map[string]*types.Func {
	r := make(map[string]*types.Func)
	mset := types.NewMethodSet(types.NewPointer(pkg.Scope().Lookup(typename).Type()))
	for i := 0; i < mset.Len(); i++ {
		fn := mset.At(i).Obj().(*types.Func)
		r[fn.Name()] = fn
	}
	return r
}

func qf(*types.Package) string {
	return ""
}

func publicMethodOf(decl ast.Decl, receiver string) *ast.FuncDecl {
	fndecl, isfunc := decl.(*ast.FuncDecl)
	if !isfunc {
		return nil
	}
	if fndecl.Name.Name[0] >= 'a' && fndecl.Name.Name[0] <= 'z' {
		return nil
	}
	if fndecl.Recv == nil || len(fndecl.Recv.List) != 1 {
		return nil
	}
	starexpr, isstar := fndecl.Recv.List[0].Type.(*ast.StarExpr)
	if !isstar {
		return nil
	}
	identexpr, isident := starexpr.X.(*ast.Ident)
	if !isident || identexpr.Name != receiver {
		return nil
	}
	if fndecl.Body == nil {
		return nil
	}
	return fndecl
}

func findCallCall(fndecl *ast.FuncDecl) *ast.CallExpr {
	for _, stmt := range fndecl.Body.List {
		var x ast.Expr = nil

		switch s := stmt.(type) {
		case *ast.AssignStmt:
			if len(s.Rhs) == 1 {
				x = s.Rhs[0]
			}
		case *ast.ReturnStmt:
			if len(s.Results) == 1 {
				x = s.Results[0]
			}
		case *ast.ExprStmt:
			x = s.X
		}

		callx, iscall := x.(*ast.CallExpr)
		if !iscall {
			continue
		}
		fun, issel := callx.Fun.(*ast.SelectorExpr)
		if !issel || (fun.Sel.Name != "call" && fun.Sel.Name != "callWhileDrainingEvents") {
			continue
		}
		return callx
	}
	return nil
}
