package cmd

import (
	"fmt"
	"io/ioutil"
	"path"
	"path/filepath"
	"reflect"

	"github.com/dave/jennifer/jen"
	"github.com/influxdata/flux/ast"
	"github.com/influxdata/flux/internal/token"
	"github.com/influxdata/flux/parser"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// generateCmd represents the generate command
var generateCmd = &cobra.Command{
	Use:   "generate",
	Short: "Generate Go source from Flux source",
	Long: `This utility creates Go sources files from Flux source files.
The process is to parse directories recursively and within each directory
write out a single file with the Flux AST representation of the directory source.
`,
	RunE: generate,
}

var pkgName string
var rootDir string
var importFile string

func init() {
	rootCmd.AddCommand(generateCmd)
	generateCmd.Flags().StringVar(&pkgName, "pkg", "", "The fully qualified package name of the root package.")
	generateCmd.Flags().StringVar(&rootDir, "root-dir", ".", "The root level directory for all packages.")
	generateCmd.Flags().StringVar(&importFile, "import-file", "builtin_gen.go", "Location relative to root-dir to place a file to import all generated packages.")
}

func generate(cmd *cobra.Command, args []string) error {
	var goPackages []string
	err := walkDirs(rootDir, func(dir string) error {
		fset := new(token.FileSet)
		pkgs, err := parser.ParseDir(fset, dir)
		if err != nil {
			return err
		}
		var pkg *ast.Package
		switch len(pkgs) {
		case 0:
			return nil
		case 1:
			for k := range pkgs {
				pkg = pkgs[k]
			}
		default:
			keys := make([]string, 0, len(pkgs))
			for k := range pkgs {
				keys = append(keys, k)
			}
			return fmt.Errorf("found multiple packages in the same directory: %s packages %v", dir, keys)
		}
		if ast.Check(pkg) > 0 {
			return errors.Wrapf(ast.GetError(pkg), "failed to parse package %q", pkg.Package)
		}

		pkgPath := path.Join(pkgName, dir)
		if pkgPath != pkgName {
			goPackages = append(goPackages, pkgPath)
		}

		// Assign the absolute package path
		path, err := filepath.Rel(rootDir, dir)
		if err != nil {
			return err
		}
		pkg.Path = path

		// Write out the package
		f := jen.NewFile(pkg.Package)
		f.HeaderComment("// DO NOT EDIT: This file is autogenerated via the builtin command.")
		f.Func().Id("init").Call().Block(
			jen.Qual("github.com/influxdata/flux", "RegisterPackage").
				Call(
					jen.Id("pkgAST"),
				),
		)
		// Construct a value using reflection for the pkg AST
		v, err := constructValue(reflect.ValueOf(pkg))
		if err != nil {
			return err
		}
		f.Var().Id("pkgAST").Op("=").Add(v)

		return f.Save(filepath.Join(dir, "flux_gen.go"))
	})
	if err != nil {
		return err
	}

	// Write the import file
	f := jen.NewFile(path.Base(pkgName))
	f.HeaderComment("// DO NOT EDIT: This file is autogenerated via the builtin command.")
	f.Anon(goPackages...)
	return f.Save(filepath.Join(rootDir, importFile))
}

func walkDirs(path string, f func(dir string) error) error {
	files, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}
	if err := f(path); err != nil {
		return err
	}

	for _, file := range files {
		if file.IsDir() {
			if err := walkDirs(filepath.Join(path, file.Name()), f); err != nil {
				return err
			}
		}
	}
	return nil
}

// indirectType returns a code statement that represents the type expression
// for the given type.
func indirectType(typ reflect.Type) *jen.Statement {
	switch typ.Kind() {
	case reflect.Map:
		c := jen.Index(indirectType(typ.Key()))
		c.Add(indirectType(typ.Elem()))
		return c
	case reflect.Ptr:
		c := jen.Op("*")
		c.Add(indirectType(typ.Elem()))
		return c
	case reflect.Array, reflect.Slice:
		c := jen.Index()
		c.Add(indirectType(typ.Elem()))
		return c
	default:
		return jen.Qual(typ.PkgPath(), typ.Name())
	}
}

// constructValue returns a Code value for the given value.
func constructValue(v reflect.Value) (jen.Code, error) {
	switch v.Kind() {
	case reflect.Array:
		s := indirectType(v.Type())
		values := make([]jen.Code, v.Len())
		for i := 0; i < v.Len(); i++ {
			val, err := constructValue(v.Index(i))
			if err != nil {
				return nil, err
			}
			values[i] = val
		}
		s.Values(values...)
		return s, nil
	case reflect.Slice:
		if v.IsNil() {
			return jen.Nil(), nil
		}
		s := indirectType(v.Type())
		values := make([]jen.Code, v.Len())
		for i := 0; i < v.Len(); i++ {
			val, err := constructValue(v.Index(i))
			if err != nil {
				return nil, err
			}
			values[i] = val
		}
		s.Values(values...)
		return s, nil
	case reflect.Interface:
		if v.IsNil() {
			return jen.Nil(), nil
		}
		return constructValue(v.Elem())
	case reflect.Ptr:
		if v.IsNil() {
			return jen.Nil(), nil
		}
		s := jen.Op("&")
		val, err := constructValue(reflect.Indirect(v))
		if err != nil {
			return nil, err
		}
		return s.Add(val), nil
	case reflect.Map:
		if v.IsNil() {
			return jen.Nil(), nil
		}
		s := indirectType(v.Type())
		keys := v.MapKeys()
		values := make(jen.Dict, v.Len())
		for _, k := range keys {
			key, err := constructValue(k)
			if err != nil {
				return nil, err
			}
			val, err := constructValue(v.MapIndex(k))
			if err != nil {
				return nil, err
			}
			values[key] = val
		}
		s.Values(values)
		return s, nil
	case reflect.Struct:
		typ := v.Type()
		s := indirectType(typ)
		values := make(jen.Dict, v.NumField())
		for i := 0; i < v.NumField(); i++ {
			field := v.Field(i)
			if !field.CanInterface() {
				// Ignore private fields
				continue
			}

			val, err := constructValue(field)
			if err != nil {
				return nil, err
			}
			values[jen.Id(typ.Field(i).Name)] = val
		}
		s.Values(values)
		return s, nil
	case reflect.Bool,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64,
		reflect.Uint,
		reflect.Uint8,
		reflect.Uint16,
		reflect.Uint32,
		reflect.Uint64,
		reflect.Uintptr,
		reflect.Float32,
		reflect.Float64,
		reflect.Complex64,
		reflect.Complex128,
		reflect.String:
		typ := types[v.Kind()]
		cv := v.Convert(typ)
		return jen.Lit(cv.Interface()), nil
	default:
		return nil, fmt.Errorf("unsupport value kind %v", v.Kind())
	}
}

// types is map of reflect.Kind to reflect.Type for the primitive types
var types = map[reflect.Kind]reflect.Type{
	reflect.Bool:       reflect.TypeOf(false),
	reflect.Int:        reflect.TypeOf(int(0)),
	reflect.Int8:       reflect.TypeOf(int8(0)),
	reflect.Int16:      reflect.TypeOf(int16(0)),
	reflect.Int32:      reflect.TypeOf(int32(0)),
	reflect.Int64:      reflect.TypeOf(int64(0)),
	reflect.Uint:       reflect.TypeOf(uint(0)),
	reflect.Uint8:      reflect.TypeOf(uint8(0)),
	reflect.Uint16:     reflect.TypeOf(uint16(0)),
	reflect.Uint32:     reflect.TypeOf(uint32(0)),
	reflect.Uint64:     reflect.TypeOf(uint64(0)),
	reflect.Uintptr:    reflect.TypeOf(uintptr(0)),
	reflect.Float32:    reflect.TypeOf(float32(0)),
	reflect.Float64:    reflect.TypeOf(float64(0)),
	reflect.Complex64:  reflect.TypeOf(complex64(0)),
	reflect.Complex128: reflect.TypeOf(complex128(0)),
	reflect.String:     reflect.TypeOf(""),
}
