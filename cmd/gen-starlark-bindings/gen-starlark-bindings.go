package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"log"
	"os"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

// getSuitableMethods returns the list of methods of service/rpc2.RPCServer that are exported as API calls
func getSuitableMethods(pkg *types.Package, typename string, skiplist []string) []*types.Func {
	r := []*types.Func{}
	mset := types.NewMethodSet(types.NewPointer(pkg.Scope().Lookup(typename).Type()))
methodLoop:
	for i := 0; i < mset.Len(); i++ {
		fn := mset.At(i).Obj().(*types.Func)

		if !fn.Exported() {
			continue
		}

		if fn.Name() == "Command" || fn.Name() == "Restart" || fn.Name() == "State" {
			r = append(r, fn)
			continue
		}

		for _, toskip := range skiplist {
			if fn.Name() == toskip {
				continue methodLoop
			}
		}

		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			continue
		}

		// arguments must be (args, *reply)
		if sig.Params().Len() != 2 {
			continue
		}
		if ntyp, isname := sig.Params().At(0).Type().(*types.Named); !isname {
			continue
		} else if _, isstr := ntyp.Underlying().(*types.Struct); !isstr {
			continue
		}
		if _, isptr := sig.Params().At(1).Type().(*types.Pointer); !isptr {
			continue
		}

		// return values must be (error)
		if sig.Results().Len() != 1 {
			continue
		}
		if sig.Results().At(0).Type().String() != "error" {
			continue
		}

		r = append(r, fn)
	}
	return r
}

func fieldsOfStruct(typ types.Type) (fieldNames, fieldTypes []string) {
	styp := typ.(*types.Named).Underlying().(*types.Struct)
	for i := 0; i < styp.NumFields(); i++ {
		fieldNames = append(fieldNames, styp.Field(i).Name())
		fieldTypes = append(fieldTypes, styp.Field(i).Type().String())
	}
	return fieldNames, fieldTypes
}

func camelToDash(in string) string {
	out := []rune{}
	for i, ch := range in {
		isupper := func(i int) bool {
			ch := in[i]
			return ch >= 'A' && ch <= 'Z'
		}

		if i > 0 && isupper(i) {
			if !isupper(i - 1) {
				out = append(out, '_')
			} else if i+1 < len(in) && !isupper(i+1) {
				out = append(out, '_')
			}
		}
		out = append(out, unicode.ToLower(ch))
	}
	return string(out)
}

type binding struct {
	name string
	fn   *types.Func

	argType, retType string

	argNames      []string
	argTypes      []string
	needsCallback bool

	docStr string
}

func processServerMethods(serverMethods []*types.Func, funcDeclByPos map[token.Pos]*ast.FuncDecl) []binding {
	bindings := make([]binding, len(serverMethods))
	for i, fn := range serverMethods {
		sig, _ := fn.Type().(*types.Signature)
		argNames, argTypes := fieldsOfStruct(sig.Params().At(0).Type())

		name := camelToDash(fn.Name())

		switch name {
		case "set":
			// avoid collision with builtin that already exists in starlark
			name = "set_expr"
		case "command":
			name = "raw_command"
		default:
			// remove list_ prefix, it looks better
			const listPrefix = "list_"
			if strings.HasPrefix(name, listPrefix) {
				name = name[len(listPrefix):]
			}
		}

		retType := sig.Params().At(1).Type().String()
		needsCallback := true
		switch fn.Name() {
		case "Command":
			retType = "CommandOut"
		case "Restart":
			retType = "RestartOut"
		case "State":
			retType = "StateOut"
		default:
			needsCallback = false
		}

		docStr := ""

		if decl := funcDeclByPos[fn.Pos()]; decl != nil && decl.Doc != nil {
			docs := []string{}
			for _, cmnt := range decl.Doc.List {
				docs = append(docs, strings.TrimPrefix(strings.TrimPrefix(cmnt.Text, "//"), " "))
			}

			// fix name of the function in the first line of the documentation
			if fields := strings.SplitN(docs[0], " ", 2); len(fields) == 2 && fields[0] == fn.Name() {
				docs[0] = name + " " + fields[1]
			}

			docStr = strings.Join(docs, "\n")
		}

		bindings[i] = binding{
			name:          name,
			fn:            fn,
			argType:       sig.Params().At(0).Type().String(),
			retType:       retType,
			argNames:      argNames,
			argTypes:      argTypes,
			docStr:        docStr,
			needsCallback: needsCallback,
		}
	}
	return bindings
}

func removePackagePathAndRPC2(typePath string) string {
	lastSlash := strings.LastIndex(typePath, "/")
	if lastSlash < 0 {
		return typePath
	}
	typePath = typePath[lastSlash+1:]
	firstDot := strings.Index(typePath, ".")
	if firstDot < 0 || typePath[:firstDot] != "rpc2" {
		return typePath
	}
	return typePath[firstDot+1:]
}

func genMapping(bindings []binding) []byte {
	buf := bytes.NewBuffer([]byte{})

	fmt.Fprintf(buf, "// DO NOT EDIT: auto-generated using github.com/go-delve/build-tools/cmd/gen-starlark-bindings\n\n")
	fmt.Fprintf(buf, "package rpc2\n\n")
	fmt.Fprintf(buf, `import (
		"go.starlark.net/starlark"
		"github.com/go-delve/delve/service/api"
		"github.com/go-delve/delve/service/debugger"
		"fmt"
		"errors"
	)
	
	var errNoServer = errors.New("no server")
	
	type starlarkRPCCallback struct {
		rpcRet any
		err    error
	}
	
	func (*starlarkRPCCallback) DisconnectChan() chan struct{} {
		return nil
	}
	
	func (*starlarkRPCCallback) SetupDoneChan() chan struct{} {
		return nil
	}
	
	func (cb *starlarkRPCCallback) Return(out any, err error) {
		cb.rpcRet = out
		cb.err = err
	}
	`)
	fmt.Fprintf(buf, "func starlarkPredeclare(env *debugger.StarlarkEnv) (starlark.StringDict, map[string]string) {\n")
	fmt.Fprintf(buf, "r := starlark.StringDict{}\n")
	fmt.Fprintf(buf, "doc := make(map[string]string)\n\n")

	for _, binding := range bindings {
		fmt.Fprintf(buf, "r[%q] = starlark.NewBuiltin(%q, func(thread *starlark.Thread, _ *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {", binding.name, binding.name)
		fmt.Fprintf(buf, "if err := debugger.StarlarkThreadIsCancelled(thread); err != nil { return starlark.None, debugger.DecorateStarlarkError(thread, err) }\n")
		fmt.Fprintf(buf, "s, hasServer := thread.Local(debugger.DlvThreadLocalRPCServer).(*RPCServer)\n")
		fmt.Fprintf(buf, "if !hasServer { return starlark.None, debugger.DecorateStarlarkError(thread, errNoServer) }\n")
		fmt.Fprintf(buf, "var rpcArgs %s\n", removePackagePathAndRPC2(binding.argType))
		if !binding.needsCallback {
			fmt.Fprintf(buf, "var rpcRet %s\n", removePackagePathAndRPC2(binding.retType))
		}

		// unmarshal normal unnamed arguments
		for i := range binding.argNames {
			fmt.Fprintf(buf, "if len(args) > %d && args[%d] != starlark.None { err := debugger.UnmarshalStarlarkValue(args[%d], &rpcArgs.%s, %q); if err != nil { return starlark.None, debugger.DecorateStarlarkError(thread, err) } }", i, i, i, binding.argNames[i], binding.argNames[i])

			switch binding.argTypes[i] {
			case "*github.com/go-delve/delve/service/api.LoadConfig":
				if binding.fn.Name() != "Stacktrace" {
					fmt.Fprintf(buf, "else { cfg := env.LoadConfig(); rpcArgs.%s = &cfg }", binding.argNames[i])
				}
			case "github.com/go-delve/delve/service/api.LoadConfig":
				fmt.Fprintf(buf, "else { rpcArgs.%s = env.LoadConfig() }", binding.argNames[i])
			case "*github.com/go-delve/delve/service/api.EvalScope":
				fmt.Fprintf(buf, "else { scope := env.Scope(); rpcArgs.%s = &scope }", binding.argNames[i])
			case "github.com/go-delve/delve/service/api.EvalScope":
				fmt.Fprintf(buf, "else { rpcArgs.%s = env.Scope() }", binding.argNames[i])
			}

			fmt.Fprintf(buf, "\n")
		}

		// unmarshal keyword arguments
		if len(binding.argNames) > 0 {
			fmt.Fprintf(buf, "for _, kv := range kwargs {\n")
			fmt.Fprintf(buf, "var err error\n")
			fmt.Fprintf(buf, "switch kv[0].(starlark.String) {\n")
			for i := range binding.argNames {
				fmt.Fprintf(buf, "case %q: ", binding.argNames[i])
				fmt.Fprintf(buf, "err = debugger.UnmarshalStarlarkValue(kv[1], &rpcArgs.%s, %q)\n", binding.argNames[i], binding.argNames[i])
			}
			fmt.Fprintf(buf, "default: err = fmt.Errorf(\"unknown argument %%q\", kv[0])")
			fmt.Fprintf(buf, "}\n")
			fmt.Fprintf(buf, "if err != nil { return starlark.None, debugger.DecorateStarlarkError(thread, err) }\n")
			fmt.Fprintf(buf, "}\n")
		}

		if binding.needsCallback {
			fmt.Fprintf(buf, "cb := &starlarkRPCCallback{}\n")
			fmt.Fprintf(buf, "s.%s(rpcArgs, cb)\n", binding.fn.Name())
			fmt.Fprintf(buf, "if cb.err != nil { return starlark.None, cb.err }\n")
			fmt.Fprintf(buf, "return env.InterfaceToStarlarkValue(cb.rpcRet), nil\n")
		} else {
			fmt.Fprintf(buf, "err := s.%s(rpcArgs, &rpcRet)\n", binding.fn.Name())
			fmt.Fprintf(buf, "if err != nil { return starlark.None, err }\n")
			fmt.Fprintf(buf, "return env.InterfaceToStarlarkValue(&rpcRet), nil\n")
		}

		fmt.Fprintf(buf, "})\n")

		// builtin documentation
		docstr := new(strings.Builder)
		fmt.Fprintf(docstr, "builtin %s(", binding.name)
		for i, argname := range binding.argNames {
			if i != 0 {
				fmt.Fprintf(docstr, ", ")
			}
			fmt.Fprint(docstr, argname)
		}
		fmt.Fprintf(docstr, ")")
		if binding.docStr != "" {
			fmt.Fprintf(docstr, "\n\n%s", binding.docStr)
		}
		fmt.Fprintf(buf, "doc[%q] = %q\n", binding.name, docstr.String())
	}

	fmt.Fprintf(buf, "return r, doc\n")
	fmt.Fprintf(buf, "}\n")

	return buf.Bytes()
}

func genDocs(bindings []binding) []byte {
	var buf bytes.Buffer

	fmt.Fprintf(&buf, "Function | API Call\n")
	fmt.Fprintf(&buf, "---------|---------\n")

	for _, binding := range bindings {
		argNames := strings.Join(binding.argNames, ", ")
		fmt.Fprintf(&buf, "%s(%s) | Equivalent to API call [%s](https://pkg.go.dev/github.com/go-delve/delve/service/rpc2#RPCServer.%s)\n", binding.name, argNames, binding.fn.Name(), binding.fn.Name())
	}

	fmt.Fprintf(&buf, "dlv_command(command) | Executes the specified command as if typed at the dlv_prompt\n")
	fmt.Fprintf(&buf, "read_file(path) | Reads the file as a string\n")
	fmt.Fprintf(&buf, "write_file(path, contents) | Writes string to a file\n")
	fmt.Fprintf(&buf, "cur_scope() | Returns the current evaluation scope\n")
	fmt.Fprintf(&buf, "default_load_config() | Returns the current default load configuration\n")

	return buf.Bytes()
}

const (
	startOfMappingTable = "<!-- BEGIN MAPPING TABLE -->"
	endOfMappingTable   = "<!-- END MAPPING TABLE -->"
)

func spliceDocs(docpath string, docs []byte, outpath string) {
	docbuf, err := os.ReadFile(docpath)
	if err != nil {
		log.Fatalf("could not read doc file: %v", err)
	}

	v := strings.Split(string(docbuf), startOfMappingTable)
	if len(v) != 2 {
		log.Fatal("could not find start of mapping table")
	}
	header := v[0]
	v = strings.Split(v[1], endOfMappingTable)
	if len(v) != 2 {
		log.Fatal("could not find end of mapping table")
	}
	footer := v[1]

	outbuf := make([]byte, 0, len(header)+len(docs)+len(footer)+len(startOfMappingTable)+len(endOfMappingTable)+1)
	outbuf = append(outbuf, []byte(header)...)
	outbuf = append(outbuf, []byte(startOfMappingTable)...)
	outbuf = append(outbuf, '\n')
	outbuf = append(outbuf, docs...)
	outbuf = append(outbuf, []byte(endOfMappingTable)...)
	outbuf = append(outbuf, []byte(footer)...)

	if outpath != "-" {
		err = os.WriteFile(outpath, outbuf, 0o664)
		if err != nil {
			log.Fatalf("could not write documentation file: %v", err)
		}
	} else {
		os.Stdout.Write(outbuf)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, "gen-starlark-bindings [doc|doc/dummy|go] <destination file> <skiplist>\n")
	fmt.Fprintf(os.Stderr, "gen-starlark-bindings version\n\n")
	fmt.Fprintf(os.Stderr, "Writes starlark documentation (doc) or mapping file (go) to <destination file>. Specify doc/dummy to generated documentation without overwriting the destination file.\nThe skiplist parameter is a comma separated list of API entrypoints to be skipped.\n\nIf passed 'version' prints the script version.\n")
	os.Exit(1)
}

func main() {
	if len(os.Args) != 4 {
		if len(os.Args) == 2 && os.Args[1] == "version" {
			fmt.Println("2")
			return
		}
		usage()
	}
	kind := os.Args[1]
	path := os.Args[2]
	skiplist := strings.Split(os.Args[3], ",")

	fset := &token.FileSet{}
	cfg := &packages.Config{
		Mode: packages.LoadSyntax,
		Fset: fset,
	}
	pkgs, err := packages.Load(cfg, "github.com/go-delve/delve/service/rpc2")
	if err != nil {
		log.Fatalf("could not load packages: %v", err)
	}
	if packages.PrintErrors(pkgs) > 0 {
		os.Exit(1)
	}

	var serverMethods []*types.Func
	funcDeclByPos := make(map[token.Pos]*ast.FuncDecl)
	packages.Visit(pkgs, func(pkg *packages.Package) bool {
		if pkg.PkgPath == "github.com/go-delve/delve/service/rpc2" {
			serverMethods = getSuitableMethods(pkg.Types, "RPCServer", skiplist)
		}
		for _, file := range pkg.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				if n, ok := n.(*ast.FuncDecl); ok {
					funcDeclByPos[n.Name.Pos()] = n
				}
				return true
			})
		}
		return true
	}, nil)

	bindings := processServerMethods(serverMethods, funcDeclByPos)

	switch kind {
	case "go":
		if len(os.Args) != 4 {
			usage()
		}

		mapping := genMapping(bindings)

		outfh := os.Stdout
		if path != "-" {
			outfh, err = os.Create(path)
			if err != nil {
				log.Fatalf("could not create output file: %v", err)
			}
			defer outfh.Close()
		}

		src, err := format.Source(mapping)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s", string(mapping))
			log.Fatal(err)
		}
		outfh.Write(src)

	case "doc":
		docs := genDocs(bindings)
		spliceDocs(path, docs, path)

	case "doc/dummy":
		docs := genDocs(bindings)
		spliceDocs(path, docs, "-")

	default:
		usage()
	}
}
