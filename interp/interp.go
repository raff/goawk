// Package interp is the GoAWK interpreter (a simple tree-walker).
//
// For basic usage, use the Exec function. For more complicated use
// cases and configuration options, first use the parser package to
// parse the AWK source, and then use ExecProgram to execute it with
// a specific configuration.
//
package interp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/benhoyt/goawk/internal/ast"
	. "github.com/benhoyt/goawk/lexer"
	"github.com/benhoyt/goawk/parser"
)

var (
	errExit     = errors.New("exit")
	errBreak    = errors.New("break")
	errContinue = errors.New("continue")
	errNext     = errors.New("next")

	crlfNewline = runtime.GOOS == "windows"
	varRegex    = regexp.MustCompile(`^([_a-zA-Z][_a-zA-Z0-9]*)=(.*)`)
)

// Error (actually *Error) is returned by Exec and Eval functions on
// interpreter error, for example a negative field index.
type Error struct {
	message string
}

func (e *Error) Error() string {
	return e.message
}

func newError(format string, args ...interface{}) error {
	return &Error{fmt.Sprintf(format, args...)}
}

type returnValue struct {
	Value value
}

func (r returnValue) Error() string {
	return "<return " + r.Value.str("%.6g") + ">"
}

type interp struct {
	// Input/output
	output        io.Writer
	errorOutput   io.Writer
	scanner       *bufio.Scanner
	scanners      map[string]*bufio.Scanner
	stdin         io.Reader
	filenameIndex int
	hadFiles      bool
	input         io.Reader
	inputStreams  map[string]io.ReadCloser
	outputStreams map[string]io.WriteCloser
	commands      map[string]*exec.Cmd
	noExec        bool
	noFileWrites  bool
	noFileReads   bool
	shellCommand  []string

	// Scalars, arrays, and function state
	globals     []value
	stack       []value
	frame       []value
	arrays      []map[string]value
	localArrays [][]int
	callDepth   int
	nativeFuncs []nativeFunc

	// File, line, and field handling
	filename        value
	line            string
	lineIsTrueStr   bool
	lineNum         int
	fileLineNum     int
	fields          []string
	fieldsIsTrueStr []bool
	numFields       int
	haveFields      bool

	// Built-in variables
	argc             int
	convertFormat    string
	outputFormat     string
	fieldSep         string
	fieldSepRegex    *regexp.Regexp
	recordSep        string
	recordSepRegex   *regexp.Regexp
	recordTerminator string
	outputFieldSep   string
	outputRecordSep  string
	subscriptSep     string
	matchLength      int
	matchStart       int

	// Misc pieces of state
	program     *parser.Program
	random      *rand.Rand
	randSeed    float64
	exitStatus  int
	regexCache  map[string]*regexp.Regexp
	formatCache map[string]cachedFormat
	bytes       bool

	// TODO: for compiled virtual machine
	// TODO: consider changing to pre-allocated array/slice with separate stack pointer - slightly faster in my tests
	st []value
}

// Various const configuration. Could make these part of Config if
// we wanted to, but no need for now.
const (
	maxCachedRegexes = 100
	maxCachedFormats = 100
	maxRecordLength  = 10 * 1024 * 1024 // 10MB seems like plenty
	maxFieldIndex    = 1000000
	maxCallDepth     = 1000
	initialStackSize = 100
	outputBufSize    = 64 * 1024
	inputBufSize     = 64 * 1024
)

// Config defines the interpreter configuration for ExecProgram.
type Config struct {
	// Standard input reader (defaults to os.Stdin)
	Stdin io.Reader

	// Writer for normal output (defaults to a buffered version of
	// os.Stdout)
	Output io.Writer

	// Writer for non-fatal error messages (defaults to os.Stderr)
	Error io.Writer

	// The name of the executable (accessible via ARGV[0])
	Argv0 string

	// Input arguments (usually filenames): empty slice means read
	// only from Stdin, and a filename of "-" means read from Stdin
	// instead of a real file.
	Args []string

	// List of name-value pairs for variables to set before executing
	// the program (useful for setting FS and other built-in
	// variables, for example []string{"FS", ",", "OFS", ","}).
	Vars []string

	// Map of named Go functions to allow calling from AWK. You need
	// to pass this same map to the parser.ParseProgram config.
	//
	// Functions can have any number of parameters, and variadic
	// functions are supported. Functions can have no return values,
	// one return value, or two return values (result, error). In the
	// two-value case, if the function returns a non-nil error,
	// program execution will stop and ExecProgram will return that
	// error.
	//
	// Apart from the error return value, the types supported are
	// bool, integer and floating point types (excluding complex),
	// and string types (string or []byte).
	//
	// It's not an error to call a Go function from AWK with fewer
	// arguments than it has parameters in Go. In this case, the zero
	// value will be used for any additional parameters. However, it
	// is a parse error to call a non-variadic function from AWK with
	// more arguments than it has parameters in Go.
	//
	// Functions defined with the "function" keyword in AWK code
	// take precedence over functions in Funcs.
	Funcs map[string]interface{}

	// Set one or more of these to true to prevent unsafe behaviours,
	// useful when executing untrusted scripts:
	//
	// * NoExec prevents system calls via system() or pipe operator
	// * NoFileWrites prevents writing to files via '>' or '>>'
	// * NoFileReads prevents reading from files via getline or the
	//   filenames in Args
	NoExec       bool
	NoFileWrites bool
	NoFileReads  bool

	// Exec args used to run system shell. Typically, this will
	// be {"/bin/sh", "-c"}
	ShellCommand []string

	// List of name-value pairs to be assigned to the ENVIRON special
	// array, for example []string{"USER", "bob", "HOME", "/home/bob"}.
	// If nil (the default), values from os.Environ() are used.
	Environ []string

	// Set to true to use byte indexes instead of character indexes for
	// the index, length, match, and substr functions. Note: the default
	// was changed from bytes to characters in GoAWK version 1.11.
	Bytes bool
}

// Initialize program execution.
func execInit(program *parser.Program, config *Config) (*interp, error) {
	if len(config.Vars)%2 != 0 {
		return nil, newError("length of config.Vars must be a multiple of 2, not %d", len(config.Vars))
	}
	if len(config.Environ)%2 != 0 {
		return nil, newError("length of config.Environ must be a multiple of 2, not %d", len(config.Environ))
	}

	p := &interp{program: program}

	// Allocate memory for variables
	p.globals = make([]value, len(program.Scalars))
	p.stack = make([]value, 0, initialStackSize)
	p.arrays = make([]map[string]value, len(program.Arrays), len(program.Arrays)+initialStackSize)
	for i := 0; i < len(program.Arrays); i++ {
		p.arrays[i] = make(map[string]value)
	}

	// Initialize defaults
	p.regexCache = make(map[string]*regexp.Regexp, 10)
	p.formatCache = make(map[string]cachedFormat, 10)
	p.randSeed = 1.0
	seed := math.Float64bits(p.randSeed)
	p.random = rand.New(rand.NewSource(int64(seed)))
	p.convertFormat = "%.6g"
	p.outputFormat = "%.6g"
	p.fieldSep = " "
	p.recordSep = "\n"
	p.outputFieldSep = " "
	p.outputRecordSep = "\n"
	p.subscriptSep = "\x1c"
	p.noExec = config.NoExec
	p.noFileWrites = config.NoFileWrites
	p.noFileReads = config.NoFileReads
	p.bytes = config.Bytes
	err := p.initNativeFuncs(config.Funcs)
	if err != nil {
		return nil, err
	}

	// Setup ARGV and other variables from config
	argvIndex := program.Arrays["ARGV"]
	p.setArrayValue(ast.ScopeGlobal, argvIndex, "0", str(config.Argv0))
	p.argc = len(config.Args) + 1
	for i, arg := range config.Args {
		p.setArrayValue(ast.ScopeGlobal, argvIndex, strconv.Itoa(i+1), numStr(arg))
	}
	p.filenameIndex = 1
	p.hadFiles = false
	for i := 0; i < len(config.Vars); i += 2 {
		err := p.setVarByName(config.Vars[i], config.Vars[i+1])
		if err != nil {
			return nil, err
		}
	}

	// Setup ENVIRON from config or environment variables
	environIndex := program.Arrays["ENVIRON"]
	if config.Environ != nil {
		for i := 0; i < len(config.Environ); i += 2 {
			p.setArrayValue(ast.ScopeGlobal, environIndex, config.Environ[i], numStr(config.Environ[i+1]))
		}
	} else {
		for _, kv := range os.Environ() {
			eq := strings.IndexByte(kv, '=')
			if eq >= 0 {
				p.setArrayValue(ast.ScopeGlobal, environIndex, kv[:eq], numStr(kv[eq+1:]))
			}
		}
	}

	// Setup system shell command
	if len(config.ShellCommand) != 0 {
		p.shellCommand = config.ShellCommand
	} else {
		executable := "/bin/sh"
		if runtime.GOOS == "windows" {
			executable = "sh"
		}
		p.shellCommand = []string{executable, "-c"}
	}

	// Setup I/O structures
	p.stdin = config.Stdin
	if p.stdin == nil {
		p.stdin = os.Stdin
	}
	p.output = config.Output
	if p.output == nil {
		p.output = bufio.NewWriterSize(os.Stdout, outputBufSize)
	}
	p.errorOutput = config.Error
	if p.errorOutput == nil {
		p.errorOutput = os.Stderr
	}
	p.inputStreams = make(map[string]io.ReadCloser)
	p.outputStreams = make(map[string]io.WriteCloser)
	p.commands = make(map[string]*exec.Cmd)
	p.scanners = make(map[string]*bufio.Scanner)
	return p, nil
}

// ExecProgram executes the parsed program using the given interpreter
// config, returning the exit status code of the program. Error is nil
// on successful execution of the program, even if the program returns
// a non-zero status code.
func ExecProgram(program *parser.Program, config *Config) (int, error) {
	p, err := execInit(program, config)
	if err != nil {
		return 0, err
	}
	defer p.closeAll()

	// Execute the program! BEGIN, then pattern/actions, then END
	err = p.execBeginEnd(program.Begin)
	if err != nil && err != errExit {
		return 0, err
	}
	if program.Actions == nil && program.End == nil {
		return p.exitStatus, nil
	}
	if err != errExit {
		err = p.execActions(program.Actions)
		if err != nil && err != errExit {
			return 0, err
		}
	}
	err = p.execBeginEnd(program.End)
	if err != nil && err != errExit {
		return 0, err
	}
	return p.exitStatus, nil
}

// Exec provides a simple way to parse and execute an AWK program
// with the given field separator. Exec reads input from the given
// reader (nil means use os.Stdin) and writes output to stdout (nil
// means use a buffered version of os.Stdout).
func Exec(source, fieldSep string, input io.Reader, output io.Writer) error {
	prog, err := parser.ParseProgram([]byte(source), nil)
	if err != nil {
		return err
	}
	config := &Config{
		Stdin:  input,
		Output: output,
		Error:  ioutil.Discard,
		Vars:   []string{"FS", fieldSep},
	}
	_, err = ExecProgram(prog, config)
	return err
}

// Execute BEGIN or END blocks (may be multiple)
func (p *interp) execBeginEnd(beginEnd []ast.Stmts) error {
	for _, statements := range beginEnd {
		err := p.executes(statements)
		if err != nil {
			return err
		}
	}
	return nil
}

// Execute pattern-action blocks (may be multiple)
func (p *interp) execActions(actions []ast.Action) error {
	inRange := make([]bool, len(actions))
lineLoop:
	for {
		// Read and setup next line of input
		line, err := p.nextLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		p.setLine(line, false)

		// Execute all the pattern-action blocks for each line
		for i, action := range actions {
			// First determine whether the pattern matches
			matched := false
			switch len(action.Pattern) {
			case 0:
				// No pattern is equivalent to pattern evaluating to true
				matched = true
			case 1:
				// Single boolean pattern
				v, err := p.eval(action.Pattern[0])
				if err != nil {
					return err
				}
				matched = v.boolean()
			case 2:
				// Range pattern (matches between start and stop lines)
				if !inRange[i] {
					v, err := p.eval(action.Pattern[0])
					if err != nil {
						return err
					}
					inRange[i] = v.boolean()
				}
				matched = inRange[i]
				if inRange[i] {
					v, err := p.eval(action.Pattern[1])
					if err != nil {
						return err
					}
					inRange[i] = !v.boolean()
				}
			}
			if !matched {
				continue
			}

			// No action is equivalent to { print $0 }
			if action.Stmts == nil {
				err := p.printLine(p.output, p.line)
				if err != nil {
					return err
				}
				continue
			}

			// Execute the body statements
			err := p.executes(action.Stmts)
			if err == errNext {
				// "next" statement skips straight to next line
				continue lineLoop
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Execute a block of multiple statements
func (p *interp) executes(stmts ast.Stmts) error {
	for _, s := range stmts {
		err := p.execute(s)
		if err != nil {
			return err
		}
	}
	return nil
}

// Execute a single statement
func (p *interp) execute(stmt ast.Stmt) error {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		// Expression statement: simply throw away the expression value
		_, err := p.eval(s.Expr)
		return err

	case *ast.PrintStmt:
		// Print OFS-separated args followed by ORS (usually newline)
		var line string
		if len(s.Args) > 0 {
			strs := make([]string, len(s.Args))
			for i, a := range s.Args {
				v, err := p.eval(a)
				if err != nil {
					return err
				}
				strs[i] = v.str(p.outputFormat)
			}
			line = strings.Join(strs, p.outputFieldSep)
		} else {
			// "print" with no args is equivalent to "print $0"
			line = p.line
		}
		output := p.output
		if s.Redirect != ILLEGAL { // token "ILLEGAL" means send to standard output
			dest, err := p.eval(s.Dest)
			if err != nil {
				return err
			}
			out, err := p.getOutputStream(s.Redirect, dest)
			if err != nil {
				return err
			}
			output = out
		}
		return p.printLine(output, line)

	case *ast.PrintfStmt:
		// printf(fmt, arg1, arg2, ...): uses our version of sprintf
		// to build the formatted string and then print that
		formatValue, err := p.eval(s.Args[0])
		if err != nil {
			return err
		}
		format := p.toString(formatValue)
		args := make([]value, len(s.Args)-1)
		for i, a := range s.Args[1:] {
			args[i], err = p.eval(a)
			if err != nil {
				return err
			}
		}
		output := p.output
		if s.Redirect != ILLEGAL { // token "ILLEGAL" means send to standard output
			dest, err := p.eval(s.Dest)
			if err != nil {
				return err
			}
			out, err := p.getOutputStream(s.Redirect, dest)
			if err != nil {
				return err
			}
			output = out
		}
		str, err := p.sprintf(format, args)
		if err != nil {
			return err
		}
		err = writeOutput(output, str)
		if err != nil {
			return err
		}

	case *ast.IfStmt:
		v, err := p.eval(s.Cond)
		if err != nil {
			return err
		}
		if v.boolean() {
			return p.executes(s.Body)
		} else {
			// Doesn't do anything if s.Else is nil
			return p.executes(s.Else)
		}

	case *ast.ForStmt:
		// C-like for loop with pre-statement, cond, and post-statement
		if s.Pre != nil {
			err := p.execute(s.Pre)
			if err != nil {
				return err
			}
		}
		for {
			if s.Cond != nil {
				v, err := p.eval(s.Cond)
				if err != nil {
					return err
				}
				if !v.boolean() {
					break
				}
			}
			err := p.executes(s.Body)
			if err == errBreak {
				break
			}
			if err != nil && err != errContinue {
				return err
			}
			if s.Post != nil {
				err := p.execute(s.Post)
				if err != nil {
					return err
				}
			}
		}

	case *ast.ForInStmt:
		// Foreach-style "for (key in array)" loop
		array := p.arrays[p.getArrayIndex(s.Array.Scope, s.Array.Index)]
		for index := range array {
			err := p.setVar(s.Var.Scope, s.Var.Index, str(index))
			if err != nil {
				return err
			}
			err = p.executes(s.Body)
			if err == errBreak {
				break
			}
			if err == errContinue {
				continue
			}
			if err != nil {
				return err
			}
		}

	case *ast.ReturnStmt:
		// Return statement uses special error value which is "caught"
		// by the callUser function
		var v value
		if s.Value != nil {
			var err error
			v, err = p.eval(s.Value)
			if err != nil {
				return err
			}
		}
		return returnValue{v}

	case *ast.WhileStmt:
		// Simple "while (cond)" loop
		for {
			v, err := p.eval(s.Cond)
			if err != nil {
				return err
			}
			if !v.boolean() {
				break
			}
			err = p.executes(s.Body)
			if err == errBreak {
				break
			}
			if err == errContinue {
				continue
			}
			if err != nil {
				return err
			}
		}

	case *ast.DoWhileStmt:
		// Do-while loop (tests condition after executing body)
		for {
			err := p.executes(s.Body)
			if err == errBreak {
				break
			}
			if err == errContinue {
				continue
			}
			if err != nil {
				return err
			}
			v, err := p.eval(s.Cond)
			if err != nil {
				return err
			}
			if !v.boolean() {
				break
			}
		}

	// Break, continue, next, and exit statements
	case *ast.BreakStmt:
		return errBreak
	case *ast.ContinueStmt:
		return errContinue
	case *ast.NextStmt:
		return errNext
	case *ast.ExitStmt:
		if s.Status != nil {
			status, err := p.eval(s.Status)
			if err != nil {
				return err
			}
			p.exitStatus = int(status.num())
		}
		// Return special errExit value "caught" by top-level executor
		return errExit

	case *ast.DeleteStmt:
		if len(s.Index) > 0 {
			// Delete single key from array
			index, err := p.evalIndex(s.Index)
			if err != nil {
				return err
			}
			array := p.arrays[p.getArrayIndex(s.Array.Scope, s.Array.Index)]
			delete(array, index) // Does nothing if key isn't present
		} else {
			// Delete entire array
			array := p.arrays[p.getArrayIndex(s.Array.Scope, s.Array.Index)]
			for k := range array {
				delete(array, k)
			}
		}

	case *ast.BlockStmt:
		// Nested block (just syntax, doesn't do anything)
		return p.executes(s.Body)

	default:
		// Should never happen
		panic(fmt.Sprintf("unexpected stmt type: %T", stmt))
	}
	return nil
}

// Evaluate a single expression, return expression value and error
func (p *interp) eval(expr ast.Expr) (value, error) {
	switch e := expr.(type) {
	case *ast.NumExpr:
		// Number literal
		return num(e.Value), nil

	case *ast.StrExpr:
		// String literal
		return str(e.Value), nil

	case *ast.FieldExpr:
		// $n field expression
		index, err := p.eval(e.Index)
		if err != nil {
			return null(), err
		}
		return p.getField(int(index.num()))

	case *ast.VarExpr:
		// Variable read expression (scope is global, local, or special)
		return p.getVar(e.Scope, e.Index), nil

	case *ast.RegExpr:
		// Stand-alone /regex/ is equivalent to: $0 ~ /regex/
		re, err := p.compileRegex(e.Regex)
		if err != nil {
			return null(), err
		}
		return boolean(re.MatchString(p.line)), nil

	case *ast.BinaryExpr:
		// Binary expression. Note that && and || are special cases
		// as they're short-circuit operators.
		left, err := p.eval(e.Left)
		if err != nil {
			return null(), err
		}
		switch e.Op {
		case AND:
			if !left.boolean() {
				return num(0), nil
			}
			right, err := p.eval(e.Right)
			if err != nil {
				return null(), err
			}
			return boolean(right.boolean()), nil
		case OR:
			if left.boolean() {
				return num(1), nil
			}
			right, err := p.eval(e.Right)
			if err != nil {
				return null(), err
			}
			return boolean(right.boolean()), nil
		default:
			right, err := p.eval(e.Right)
			if err != nil {
				return null(), err
			}
			return p.evalBinary(e.Op, left, right)
		}

	case *ast.IncrExpr:
		// Pre-increment, post-increment, pre-decrement, post-decrement

		// First evaluate the expression, but remember array or field
		// index, so we don't evaluate part of the expression twice
		exprValue, arrayIndex, fieldIndex, err := p.evalForAugAssign(e.Expr)
		if err != nil {
			return null(), err
		}

		// Then convert to number and increment or decrement
		exprNum := exprValue.num()
		var incr float64
		if e.Op == INCR {
			incr = exprNum + 1
		} else {
			incr = exprNum - 1
		}
		incrValue := num(incr)

		// Finally, assign back to expression and return the correct value
		err = p.assignAug(e.Expr, arrayIndex, fieldIndex, incrValue)
		if err != nil {
			return null(), err
		}
		if e.Pre {
			return incrValue, nil
		} else {
			return num(exprNum), nil
		}

	case *ast.AssignExpr:
		// Assignment expression (returns right-hand side)
		right, err := p.eval(e.Right)
		if err != nil {
			return null(), err
		}
		err = p.assign(e.Left, right)
		if err != nil {
			return null(), err
		}
		return right, nil

	case *ast.AugAssignExpr:
		// Augmented assignment like += (returns right-hand side)
		right, err := p.eval(e.Right)
		if err != nil {
			return null(), err
		}
		left, arrayIndex, fieldIndex, err := p.evalForAugAssign(e.Left)
		if err != nil {
			return null(), err
		}
		right, err = p.evalBinary(e.Op, left, right)
		if err != nil {
			return null(), err
		}
		err = p.assignAug(e.Left, arrayIndex, fieldIndex, right)
		if err != nil {
			return null(), err
		}
		return right, nil

	case *ast.CondExpr:
		// C-like ?: ternary conditional operator
		cond, err := p.eval(e.Cond)
		if err != nil {
			return null(), err
		}
		if cond.boolean() {
			return p.eval(e.True)
		} else {
			return p.eval(e.False)
		}

	case *ast.IndexExpr:
		// Read value from array by index
		index, err := p.evalIndex(e.Index)
		if err != nil {
			return null(), err
		}
		return p.getArrayValue(e.Array.Scope, e.Array.Index, index), nil

	case *ast.CallExpr:
		// Call a builtin function
		return p.callBuiltin(e.Func, e.Args)

	case *ast.UnaryExpr:
		// Unary ! or + or -
		v, err := p.eval(e.Value)
		if err != nil {
			return null(), err
		}
		return p.evalUnary(e.Op, v), nil

	case *ast.InExpr:
		// "key in array" expression
		index, err := p.evalIndex(e.Index)
		if err != nil {
			return null(), err
		}
		array := p.arrays[p.getArrayIndex(e.Array.Scope, e.Array.Index)]
		_, ok := array[index]
		return boolean(ok), nil

	case *ast.UserCallExpr:
		// Call user-defined or native Go function
		if e.Native {
			return p.callNative(e.Index, e.Args)
		} else {
			return p.callUser(e.Index, e.Args)
		}

	case *ast.GetlineExpr:
		// Getline: read line from input
		var line string
		switch {
		case e.Command != nil:
			nameValue, err := p.eval(e.Command)
			if err != nil {
				return null(), err
			}
			name := p.toString(nameValue)
			scanner, err := p.getInputScannerPipe(name)
			if err != nil {
				return null(), err
			}
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return num(-1), nil
				}
				return num(0), nil
			}
			line = scanner.Text()
		case e.File != nil:
			nameValue, err := p.eval(e.File)
			if err != nil {
				return null(), err
			}
			name := p.toString(nameValue)
			scanner, err := p.getInputScannerFile(name)
			if err != nil {
				if _, ok := err.(*os.PathError); ok {
					// File not found is not a hard error, getline just returns -1.
					// See: https://github.com/benhoyt/goawk/issues/41
					return num(-1), nil
				}
				return null(), err
			}
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return num(-1), nil
				}
				return num(0), nil
			}
			line = scanner.Text()
		default:
			p.flushOutputAndError() // Flush output in case they've written a prompt
			var err error
			line, err = p.nextLine()
			if err == io.EOF {
				return num(0), nil
			}
			if err != nil {
				return num(-1), nil
			}
		}
		if e.Target != nil {
			err := p.assign(e.Target, numStr(line))
			if err != nil {
				return null(), err
			}
		} else {
			p.setLine(line, false)
		}
		return num(1), nil

	default:
		// Should never happen
		panic(fmt.Sprintf("unexpected expr type: %T", expr))
	}
}

func (p *interp) evalForAugAssign(expr ast.Expr) (v value, arrayIndex string, fieldIndex int, err error) {
	switch expr := expr.(type) {
	case *ast.VarExpr:
		v = p.getVar(expr.Scope, expr.Index)
	case *ast.IndexExpr:
		arrayIndex, err = p.evalIndex(expr.Index)
		if err != nil {
			return null(), "", 0, err
		}
		v = p.getArrayValue(expr.Array.Scope, expr.Array.Index, arrayIndex)
	case *ast.FieldExpr:
		index, err := p.eval(expr.Index)
		if err != nil {
			return null(), "", 0, err
		}
		fieldIndex = int(index.num())
		v, err = p.getField(fieldIndex)
		if err != nil {
			return null(), "", 0, err
		}
	}
	return v, arrayIndex, fieldIndex, nil
}

func (p *interp) assignAug(expr ast.Expr, arrayIndex string, fieldIndex int, v value) error {
	switch expr := expr.(type) {
	case *ast.VarExpr:
		return p.setVar(expr.Scope, expr.Index, v)
	case *ast.IndexExpr:
		p.setArrayValue(expr.Array.Scope, expr.Array.Index, arrayIndex, v)
	default: // *FieldExpr
		return p.setField(fieldIndex, p.toString(v))
	}
	return nil
}

// Get a variable's value by index in given scope
func (p *interp) getVar(scope ast.VarScope, index int) value {
	switch scope {
	case ast.ScopeGlobal:
		return p.globals[index]
	case ast.ScopeLocal:
		return p.frame[index]
	default: // ScopeSpecial
		switch index {
		case ast.V_NF:
			p.ensureFields()
			return num(float64(p.numFields))
		case ast.V_NR:
			return num(float64(p.lineNum))
		case ast.V_RLENGTH:
			return num(float64(p.matchLength))
		case ast.V_RSTART:
			return num(float64(p.matchStart))
		case ast.V_FNR:
			return num(float64(p.fileLineNum))
		case ast.V_ARGC:
			return num(float64(p.argc))
		case ast.V_CONVFMT:
			return str(p.convertFormat)
		case ast.V_FILENAME:
			return p.filename
		case ast.V_FS:
			return str(p.fieldSep)
		case ast.V_OFMT:
			return str(p.outputFormat)
		case ast.V_OFS:
			return str(p.outputFieldSep)
		case ast.V_ORS:
			return str(p.outputRecordSep)
		case ast.V_RS:
			return str(p.recordSep)
		case ast.V_RT:
			return str(p.recordTerminator)
		case ast.V_SUBSEP:
			return str(p.subscriptSep)
		default:
			panic(fmt.Sprintf("unexpected special variable index: %d", index))
		}
	}
}

// Set a variable by name (specials and globals only)
func (p *interp) setVarByName(name, value string) error {
	index := ast.SpecialVarIndex(name)
	if index > 0 {
		return p.setVar(ast.ScopeSpecial, index, numStr(value))
	}
	index, ok := p.program.Scalars[name]
	if ok {
		return p.setVar(ast.ScopeGlobal, index, numStr(value))
	}
	// Ignore variables that aren't defined in program
	return nil
}

// Set a variable by index in given scope to given value
func (p *interp) setVar(scope ast.VarScope, index int, v value) error {
	switch scope {
	case ast.ScopeGlobal:
		p.globals[index] = v
		return nil
	case ast.ScopeLocal:
		p.frame[index] = v
		return nil
	default: // ScopeSpecial
		switch index {
		case ast.V_NF:
			numFields := int(v.num())
			if numFields < 0 {
				return newError("NF set to negative value: %d", numFields)
			}
			if numFields > maxFieldIndex {
				return newError("NF set too large: %d", numFields)
			}
			p.ensureFields()
			p.numFields = numFields
			if p.numFields < len(p.fields) {
				p.fields = p.fields[:p.numFields]
				p.fieldsIsTrueStr = p.fieldsIsTrueStr[:p.numFields]
			}
			for i := len(p.fields); i < p.numFields; i++ {
				p.fields = append(p.fields, "")
				p.fieldsIsTrueStr = append(p.fieldsIsTrueStr, false)
			}
			p.line = strings.Join(p.fields, p.outputFieldSep)
			p.lineIsTrueStr = true
		case ast.V_NR:
			p.lineNum = int(v.num())
		case ast.V_RLENGTH:
			p.matchLength = int(v.num())
		case ast.V_RSTART:
			p.matchStart = int(v.num())
		case ast.V_FNR:
			p.fileLineNum = int(v.num())
		case ast.V_ARGC:
			p.argc = int(v.num())
		case ast.V_CONVFMT:
			p.convertFormat = p.toString(v)
		case ast.V_FILENAME:
			p.filename = v
		case ast.V_FS:
			p.fieldSep = p.toString(v)
			if utf8.RuneCountInString(p.fieldSep) > 1 { // compare to interp.ensureFields
				re, err := regexp.Compile(p.fieldSep)
				if err != nil {
					return newError("invalid regex %q: %s", p.fieldSep, err)
				}
				p.fieldSepRegex = re
			}
		case ast.V_OFMT:
			p.outputFormat = p.toString(v)
		case ast.V_OFS:
			p.outputFieldSep = p.toString(v)
		case ast.V_ORS:
			p.outputRecordSep = p.toString(v)
		case ast.V_RS:
			p.recordSep = p.toString(v)
			switch { // compare to interp.newScanner
			case len(p.recordSep) <= 1:
				// Simple cases use specialized splitters, not regex
			case utf8.RuneCountInString(p.recordSep) == 1:
				// Multi-byte unicode char falls back to regex splitter
				sep := regexp.QuoteMeta(p.recordSep) // not strictly necessary as no multi-byte chars are regex meta chars
				p.recordSepRegex = regexp.MustCompile(sep)
			default:
				re, err := regexp.Compile(p.recordSep)
				if err != nil {
					return newError("invalid regex %q: %s", p.recordSep, err)
				}
				p.recordSepRegex = re
			}
		case ast.V_RT:
			p.recordTerminator = p.toString(v)
		case ast.V_SUBSEP:
			p.subscriptSep = p.toString(v)
		default:
			panic(fmt.Sprintf("unexpected special variable index: %d", index))
		}
		return nil
	}
}

// Determine the index of given array into the p.arrays slice. Global
// arrays are just at p.arrays[index], local arrays have to be looked
// up indirectly.
func (p *interp) getArrayIndex(scope ast.VarScope, index int) int {
	if scope == ast.ScopeGlobal {
		return index
	} else {
		return p.localArrays[len(p.localArrays)-1][index]
	}
}

// Get a value from given array by key (index)
func (p *interp) getArrayValue(scope ast.VarScope, arrayIndex int, index string) value {
	resolved := p.getArrayIndex(scope, arrayIndex)
	array := p.arrays[resolved]
	v, ok := array[index]
	if !ok {
		// Strangely, per the POSIX spec, "Any other reference to a
		// nonexistent array element [apart from "in" expressions]
		// shall automatically create it."
		array[index] = v
	}
	return v
}

// Set a value in given array by key (index)
func (p *interp) setArrayValue(scope ast.VarScope, arrayIndex int, index string, v value) {
	resolved := p.getArrayIndex(scope, arrayIndex)
	p.arrays[resolved][index] = v
}

// Get the value of given numbered field, equivalent to "$index"
func (p *interp) getField(index int) (value, error) {
	if index < 0 {
		return null(), newError("field index negative: %d", index)
	}
	if index == 0 {
		if p.lineIsTrueStr {
			return str(p.line), nil
		} else {
			return numStr(p.line), nil
		}
	}
	p.ensureFields()
	if index > len(p.fields) {
		return str(""), nil
	}
	if p.fieldsIsTrueStr[index-1] {
		return str(p.fields[index-1]), nil
	} else {
		return numStr(p.fields[index-1]), nil
	}
}

// Sets a single field, equivalent to "$index = value"
func (p *interp) setField(index int, value string) error {
	if index == 0 {
		p.setLine(value, true)
		return nil
	}
	if index < 0 {
		return newError("field index negative: %d", index)
	}
	if index > maxFieldIndex {
		return newError("field index too large: %d", index)
	}
	// If there aren't enough fields, add empty string fields in between
	p.ensureFields()
	for i := len(p.fields); i < index; i++ {
		p.fields = append(p.fields, "")
		p.fieldsIsTrueStr = append(p.fieldsIsTrueStr, true)
	}
	p.fields[index-1] = value
	p.fieldsIsTrueStr[index-1] = true
	p.numFields = len(p.fields)
	p.line = strings.Join(p.fields, p.outputFieldSep)
	p.lineIsTrueStr = true
	return nil
}

// Convert value to string using current CONVFMT
func (p *interp) toString(v value) string {
	return v.str(p.convertFormat)
}

// Compile regex string (or fetch from regex cache)
func (p *interp) compileRegex(regex string) (*regexp.Regexp, error) {
	if re, ok := p.regexCache[regex]; ok {
		return re, nil
	}
	re, err := regexp.Compile(regex)
	if err != nil {
		return nil, newError("invalid regex %q: %s", regex, err)
	}
	// Dumb, non-LRU cache: just cache the first N regexes
	if len(p.regexCache) < maxCachedRegexes {
		p.regexCache[regex] = re
	}
	return re, nil
}

// Evaluate simple binary expression and return result
func (p *interp) evalBinary(op Token, l, r value) (value, error) {
	// Note: cases are ordered (very roughly) in order of frequency
	// of occurrence for performance reasons. Benchmark on common code
	// before changing the order.
	switch op {
	case ADD:
		return num(l.num() + r.num()), nil
	case SUB:
		return num(l.num() - r.num()), nil
	case EQUALS:
		ln, lIsStr := l.isTrueStr()
		rn, rIsStr := r.isTrueStr()
		if lIsStr || rIsStr {
			return boolean(p.toString(l) == p.toString(r)), nil
		} else {
			return boolean(ln == rn), nil
		}
	case LESS:
		ln, lIsStr := l.isTrueStr()
		rn, rIsStr := r.isTrueStr()
		if lIsStr || rIsStr {
			return boolean(p.toString(l) < p.toString(r)), nil
		} else {
			return boolean(ln < rn), nil
		}
	case LTE:
		ln, lIsStr := l.isTrueStr()
		rn, rIsStr := r.isTrueStr()
		if lIsStr || rIsStr {
			return boolean(p.toString(l) <= p.toString(r)), nil
		} else {
			return boolean(ln <= rn), nil
		}
	case CONCAT:
		return str(p.toString(l) + p.toString(r)), nil
	case MUL:
		return num(l.num() * r.num()), nil
	case DIV:
		rf := r.num()
		if rf == 0.0 {
			return null(), newError("division by zero")
		}
		return num(l.num() / rf), nil
	case GREATER:
		ln, lIsStr := l.isTrueStr()
		rn, rIsStr := r.isTrueStr()
		if lIsStr || rIsStr {
			return boolean(p.toString(l) > p.toString(r)), nil
		} else {
			return boolean(ln > rn), nil
		}
	case GTE:
		ln, lIsStr := l.isTrueStr()
		rn, rIsStr := r.isTrueStr()
		if lIsStr || rIsStr {
			return boolean(p.toString(l) >= p.toString(r)), nil
		} else {
			return boolean(ln >= rn), nil
		}
	case NOT_EQUALS:
		ln, lIsStr := l.isTrueStr()
		rn, rIsStr := r.isTrueStr()
		if lIsStr || rIsStr {
			return boolean(p.toString(l) != p.toString(r)), nil
		} else {
			return boolean(ln != rn), nil
		}
	case MATCH:
		re, err := p.compileRegex(p.toString(r))
		if err != nil {
			return null(), err
		}
		matched := re.MatchString(p.toString(l))
		return boolean(matched), nil
	case NOT_MATCH:
		re, err := p.compileRegex(p.toString(r))
		if err != nil {
			return null(), err
		}
		matched := re.MatchString(p.toString(l))
		return boolean(!matched), nil
	case POW:
		return num(math.Pow(l.num(), r.num())), nil
	case MOD:
		rf := r.num()
		if rf == 0.0 {
			return null(), newError("division by zero in mod")
		}
		return num(math.Mod(l.num(), rf)), nil
	default:
		panic(fmt.Sprintf("unexpected binary operation: %s", op))
	}
}

// Evaluate unary expression and return result
func (p *interp) evalUnary(op Token, v value) value {
	switch op {
	case SUB:
		return num(-v.num())
	case NOT:
		return boolean(!v.boolean())
	case ADD:
		return num(v.num())
	default:
		panic(fmt.Sprintf("unexpected unary operation: %s", op))
	}
}

// Perform an assignment: can assign to var, array[key], or $field
func (p *interp) assign(left ast.Expr, right value) error {
	switch left := left.(type) {
	case *ast.VarExpr:
		return p.setVar(left.Scope, left.Index, right)
	case *ast.IndexExpr:
		index, err := p.evalIndex(left.Index)
		if err != nil {
			return err
		}
		p.setArrayValue(left.Array.Scope, left.Array.Index, index, right)
		return nil
	case *ast.FieldExpr:
		index, err := p.eval(left.Index)
		if err != nil {
			return err
		}
		return p.setField(int(index.num()), p.toString(right))
	}
	// Shouldn't happen
	panic(fmt.Sprintf("unexpected lvalue type: %T", left))
}

// Evaluate an index expression to a string. Multi-valued indexes are
// separated by SUBSEP.
func (p *interp) evalIndex(indexExprs []ast.Expr) (string, error) {
	// Optimize the common case of a 1-dimensional index
	if len(indexExprs) == 1 {
		v, err := p.eval(indexExprs[0])
		if err != nil {
			return "", err
		}
		return p.toString(v), nil
	}

	// Up to 3-dimensional indices won't require heap allocation
	indices := make([]string, 0, 3)
	for _, expr := range indexExprs {
		v, err := p.eval(expr)
		if err != nil {
			return "", err
		}
		indices = append(indices, p.toString(v))
	}
	return strings.Join(indices, p.subscriptSep), nil
}
