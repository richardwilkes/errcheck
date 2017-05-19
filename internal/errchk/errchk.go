// Package errchk is the library used to implement the errchk command-line tool.
//
// Note: The API of this package has not been finalized and may change at any point.
package errchk

import (
	"bufio"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"golang.org/x/tools/go/loader"
)

const (
	errIgnored                    = "error ignored"
	errAssignedToBlankIdentifier  = "error assigned to blank identifier"
	errIgnoredTypeAssertionResult = "ignored type assertion results"
)

var errorType *types.Interface

func init() {
	errorType = types.Universe.Lookup("error").Type().Underlying().(*types.Interface)

}

var (
	// ErrNoGoFiles is returned when CheckPackage is run on a package with no Go source files
	ErrNoGoFiles = errors.New("package contains no go source files")
)

// UncheckedError indicates the position of an unchecked error return.
type UncheckedError struct {
	Pos    token.Position
	Reason string
	Line   string
}

// UncheckedErrors is returned from the CheckPackage function if the package contains
// any unchecked errors.
// Errors should be appended using the Append method, which is safe to use concurrently.
type UncheckedErrors struct {
	mu sync.Mutex

	// Errors is a list of all the unchecked errors in the package.
	// Printing an error reports its position within the file and the contents of the line.
	Errors []UncheckedError
}

func (e *UncheckedErrors) Append(errors ...UncheckedError) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Errors = append(e.Errors, errors...)
}

func (e *UncheckedErrors) Error() string {
	return fmt.Sprintf("%d unchecked errors", len(e.Errors))
}

// Len is the number of elements in the collection.
func (e *UncheckedErrors) Len() int { return len(e.Errors) }

// Swap swaps the elements with indexes i and j.
func (e *UncheckedErrors) Swap(i, j int) { e.Errors[i], e.Errors[j] = e.Errors[j], e.Errors[i] }

type byName struct{ *UncheckedErrors }

// Less reports whether the element with index i should sort before the element with index j.
func (e byName) Less(i, j int) bool {
	ei, ej := e.Errors[i], e.Errors[j]

	pi, pj := ei.Pos, ej.Pos

	if pi.Filename != pj.Filename {
		return pi.Filename < pj.Filename
	}
	if pi.Line != pj.Line {
		return pi.Line < pj.Line
	}
	if pi.Column != pj.Column {
		return pi.Column < pj.Column
	}

	return ei.Line < ej.Line
}

type Checker struct {
	// ignore is a map of package names to regular expressions. Identifiers from a package are
	// checked against its regular expressions and if any of the expressions match the call
	// is not checked.
	Ignore map[string]*regexp.Regexp

	// If blank is true then assignments to the blank identifier are also considered to be
	// ignored errors.
	Blank bool

	// If asserts is true then ignored type assertion results are also checked
	Asserts bool

	// build tags
	Tags []string

	Verbose bool

	// If true, checking of of _test.go files is disabled
	WithoutTests bool

	exclude map[string]bool
}

func NewChecker() *Checker {
	c := Checker{}
	c.SetExclude(map[string]bool{})
	return &c
}

func (c *Checker) SetExclude(l map[string]bool) {
	// Default exclude for stdlib functions
	c.exclude = map[string]bool{
		"crypto/rand.Read":         true,
		"(*crypto/rand.Rand).Read": true,

		"math/rand.Read":         true,
		"(*math/rand.Rand).Read": true,

		"(*bytes.Buffer).Write":       true,
		"(*bytes.Buffer).WriteByte":   true,
		"(*bytes.Buffer).WriteRune":   true,
		"(*bytes.Buffer).WriteString": true,
	}
	for k := range l {
		c.exclude[k] = true
	}
}

func (c *Checker) logf(msg string, args ...interface{}) {
	if c.Verbose {
		fmt.Fprintf(os.Stderr, msg+"\n", args...)
	}
}

func (c *Checker) load(paths ...string) (*loader.Program, error) {
	ctx := build.Default
	for _, tag := range c.Tags {
		ctx.BuildTags = append(ctx.BuildTags, tag)
	}
	loadcfg := loader.Config{
		Build: &ctx,
	}
	rest, err := loadcfg.FromArgs(paths, !c.WithoutTests)
	if err != nil {
		return nil, fmt.Errorf("could not parse arguments: %s", err)
	}
	if len(rest) > 0 {
		return nil, fmt.Errorf("unhandled extra arguments: %v", rest)
	}

	return loadcfg.Load()
}

// CheckPackages checks packages for errors.
func (c *Checker) CheckPackages(paths ...string) error {
	program, err := c.load(paths...)
	if err != nil {
		return fmt.Errorf("could not type check: %s", err)
	}

	var wg sync.WaitGroup
	u := &UncheckedErrors{}
	for _, pkgInfo := range program.InitialPackages() {
		if pkgInfo.Pkg.Path() == "unsafe" { // not a real package
			continue
		}

		wg.Add(1)

		go func(pkgInfo *loader.PackageInfo) {
			defer wg.Done()
			c.logf("Checking %s", pkgInfo.Pkg.Path())

			v := &visitor{
				prog:    program,
				pkg:     pkgInfo,
				ignore:  c.Ignore,
				blank:   c.Blank,
				asserts: c.Asserts,
				lines:   make(map[string][]string),
				exclude: c.exclude,
				errors:  []UncheckedError{},
			}

			for _, astFile := range v.pkg.Files {
				ast.Walk(v, astFile)
			}
			u.Append(v.errors...)
		}(pkgInfo)
	}

	wg.Wait()
	if u.Len() > 0 {
		sort.Sort(byName{u})
		return u
	}
	return nil
}

// visitor implements the errchk algorithm
type visitor struct {
	prog    *loader.Program
	pkg     *loader.PackageInfo
	ignore  map[string]*regexp.Regexp
	blank   bool
	asserts bool
	lines   map[string][]string
	exclude map[string]bool

	errors []UncheckedError
}

func (v *visitor) excludeCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := v.pkg.ObjectOf(sel.Sel).(*types.Func)
	if !ok {
		// Shouldn't happen, but be paranoid
		return false
	}
	// The name is fully qualified by the import path, possible type,
	// function/method name and pointer receiver.
	//
	// TODO(dh): vendored packages will have /vendor/ in their name,
	// thus not matching vendored standard library packages. If we
	// want to support vendored stdlib packages, we need to implement
	// FullName with our own logic.
	name := fn.FullName()
	return v.exclude[name]
}

func (v *visitor) ignoreCall(call *ast.CallExpr) bool {
	if v.excludeCall(call) {
		return true
	}

	// Try to get an identifier.
	// Currently only supports simple expressions:
	//     1. f()
	//     2. x.y.f()
	var id *ast.Ident
	switch exp := call.Fun.(type) {
	case (*ast.Ident):
		id = exp
	case (*ast.SelectorExpr):
		id = exp.Sel
	default:
		// eg: *ast.SliceExpr, *ast.IndexExpr
	}

	if id == nil {
		return false
	}

	// If we got an identifier for the function, see if it is ignored
	if re, ok := v.ignore[""]; ok && re.MatchString(id.Name) {
		return true
	}

	if obj := v.pkg.Uses[id]; obj != nil {
		if pkg := obj.Pkg(); pkg != nil {
			if re, ok := v.ignore[pkg.Path()]; ok {
				return re.MatchString(id.Name)
			}

			// if current package being considered is vendored, check to see if it should be ignored based
			// on the unvendored path.
			if nonVendoredPkg, ok := nonVendoredPkgPath(pkg.Path()); ok {
				if re, ok := v.ignore[nonVendoredPkg]; ok {
					return re.MatchString(id.Name)
				}
			}
		}
	}

	return false
}

// nonVendoredPkgPath returns the unvendored version of the provided package path (or returns the provided path if it
// does not represent a vendored path). The second return value is true if the provided package was vendored, false
// otherwise.
func nonVendoredPkgPath(pkgPath string) (string, bool) {
	lastVendorIndex := strings.LastIndex(pkgPath, "/vendor/")
	if lastVendorIndex == -1 {
		return pkgPath, false
	}
	return pkgPath[lastVendorIndex+len("/vendor/"):], true
}

// errorsByArg returns a slice s such that
// len(s) == number of return types of call
// s[i] == true iff return type at position i from left is an error type
func (v *visitor) errorsByArg(call *ast.CallExpr) []bool {
	switch t := v.pkg.Types[call].Type.(type) {
	case *types.Named:
		// Single return
		return []bool{isErrorType(t)}
	case *types.Pointer:
		// Single return via pointer
		return []bool{isErrorType(t)}
	case *types.Tuple:
		// Multiple returns
		s := make([]bool, t.Len())
		for i := 0; i < t.Len(); i++ {
			switch et := t.At(i).Type().(type) {
			case *types.Named:
				// Single return
				s[i] = isErrorType(et)
			case *types.Pointer:
				// Single return via pointer
				s[i] = isErrorType(et)
			default:
				s[i] = false
			}
		}
		return s
	}
	return []bool{false}
}

func (v *visitor) callReturnsError(call *ast.CallExpr) bool {
	if v.isRecover(call) {
		return true
	}
	for _, isError := range v.errorsByArg(call) {
		if isError {
			return true
		}
	}
	return false
}

// isRecover returns true if the given CallExpr is a call to the built-in recover() function.
func (v *visitor) isRecover(call *ast.CallExpr) bool {
	if fun, ok := call.Fun.(*ast.Ident); ok {
		if _, ok := v.pkg.Uses[fun].(*types.Builtin); ok {
			return fun.Name == "recover"
		}
	}
	return false
}

func (v *visitor) addErrorAtPosition(position token.Pos, reason string) {
	pos := v.prog.Fset.Position(position)
	lines, ok := v.lines[pos.Filename]
	if !ok {
		lines = readfile(pos.Filename)
		v.lines[pos.Filename] = lines
	}

	line := "??"
	if pos.Line-1 < len(lines) {
		line = strings.TrimSpace(lines[pos.Line-1])
	}
	v.errors = append(v.errors, UncheckedError{pos, reason, line})
}

func readfile(filename string) []string {
	var f, err = os.Open(filename)
	if err != nil {
		return nil
	}

	var lines []string
	var scanner = bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func (v *visitor) Visit(node ast.Node) ast.Visitor {
	switch stmt := node.(type) {
	case *ast.ExprStmt:
		if call, ok := stmt.X.(*ast.CallExpr); ok {
			if !v.ignoreCall(call) && v.callReturnsError(call) {
				v.addErrorAtPosition(call.Lparen, errIgnored)
			}
		}
	case *ast.GoStmt:
		if !v.ignoreCall(stmt.Call) && v.callReturnsError(stmt.Call) {
			v.addErrorAtPosition(stmt.Call.Lparen, errIgnored)
		}
	case *ast.DeferStmt:
		if !v.ignoreCall(stmt.Call) && v.callReturnsError(stmt.Call) {
			v.addErrorAtPosition(stmt.Call.Lparen, errIgnored)
		}
	case *ast.AssignStmt:
		if len(stmt.Rhs) == 1 {
			// single value on rhs; check against lhs identifiers
			if call, ok := stmt.Rhs[0].(*ast.CallExpr); ok {
				if !v.blank {
					break
				}
				if v.ignoreCall(call) {
					break
				}
				isError := v.errorsByArg(call)
				for i := 0; i < len(stmt.Lhs); i++ {
					if id, ok := stmt.Lhs[i].(*ast.Ident); ok {
						// We shortcut calls to recover() because errorsByArg can't
						// check its return types for errors since it returns interface{}.
						if id.Name == "_" && (v.isRecover(call) || isError[i]) {
							v.addErrorAtPosition(id.NamePos, errAssignedToBlankIdentifier)
						}
					}
				}
			} else if assert, ok := stmt.Rhs[0].(*ast.TypeAssertExpr); ok {
				if !v.asserts {
					break
				}
				if assert.Type == nil {
					// type switch
					break
				}
				if len(stmt.Lhs) < 2 {
					// assertion result not read
					v.addErrorAtPosition(stmt.Rhs[0].Pos(), errIgnoredTypeAssertionResult)
				} else if id, ok := stmt.Lhs[1].(*ast.Ident); ok && v.blank && id.Name == "_" {
					// assertion result ignored
					v.addErrorAtPosition(id.NamePos, errAssignedToBlankIdentifier)
				}
			}
		} else {
			// multiple value on rhs; in this case a call can't return
			// multiple values. Assume len(stmt.Lhs) == len(stmt.Rhs)
			for i := 0; i < len(stmt.Lhs); i++ {
				if id, ok := stmt.Lhs[i].(*ast.Ident); ok {
					if call, ok := stmt.Rhs[i].(*ast.CallExpr); ok {
						if !v.blank {
							continue
						}
						if v.ignoreCall(call) {
							continue
						}
						if id.Name == "_" && v.callReturnsError(call) {
							v.addErrorAtPosition(id.NamePos, errAssignedToBlankIdentifier)
						}
					} else if assert, ok := stmt.Rhs[i].(*ast.TypeAssertExpr); ok {
						if !v.asserts {
							continue
						}
						if assert.Type == nil {
							// Shouldn't happen anyway, no multi assignment in type switches
							continue
						}
						v.addErrorAtPosition(id.NamePos, errIgnoredTypeAssertionResult)
					}
				}
			}
		}
	default:
	}
	return v
}

func isErrorType(t types.Type) bool {
	return types.Implements(t, errorType)
}
