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
	"github.com/benhoyt/goawk/internal/compiler"
	"github.com/benhoyt/goawk/parser"
)

var (
	errExit  = errors.New("exit")
	errBreak = errors.New("break")
	errNext  = errors.New("next")

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
	sp          int
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

	// Parsed program, compiled functions and constants
	program   *parser.Program
	functions []compiler.Function
	nums      []float64
	strs      []string
	regexes   []*regexp.Regexp

	// Misc pieces of state
	random      *rand.Rand
	randSeed    float64
	exitStatus  int
	regexCache  map[string]*regexp.Regexp
	formatCache map[string]cachedFormat
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
}

// ExecProgram executes the parsed program using the given interpreter
// config, returning the exit status code of the program. Error is nil
// on successful execution of the program, even if the program returns
// a non-zero status code.
func ExecProgram(program *parser.Program, config *Config) (int, error) {
	if len(config.Vars)%2 != 0 {
		return 0, newError("length of config.Vars must be a multiple of 2, not %d", len(config.Vars))
	}
	if len(config.Environ)%2 != 0 {
		return 0, newError("length of config.Environ must be a multiple of 2, not %d", len(config.Environ))
	}

	p := &interp{
		program:   program,
		functions: program.Compiled.Functions,
		nums:      program.Compiled.Nums,
		strs:      program.Compiled.Strs,
		regexes:   program.Compiled.Regexes,
	}

	// Allocate memory for variables and virtual machine stack
	p.globals = make([]value, len(program.Scalars))
	p.stack = make([]value, initialStackSize)
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
	err := p.initNativeFuncs(config.Funcs)
	if err != nil {
		return 0, err
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
			return 0, err
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
	defer p.closeAll()

	// Execute the program: BEGIN, then pattern/actions, then END
	err = p.execute(program.Compiled.Begin)
	if err != nil && err != errExit {
		return 0, err
	}
	if program.Actions == nil && program.End == nil {
		return p.exitStatus, nil
	}
	if err != errExit {
		err = p.execActions(program.Compiled.Actions)
		if err != nil && err != errExit {
			return 0, err
		}
	}
	err = p.execute(program.Compiled.End)
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

// Execute pattern-action blocks (may be multiple)
func (p *interp) execActions(actions []compiler.Action) error {
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
				err := p.execute(action.Pattern[0])
				if err != nil {
					return err
				}
				matched = p.pop().boolean()
			case 2:
				// Range pattern (matches between start and stop lines)
				if !inRange[i] {
					err := p.execute(action.Pattern[0])
					if err != nil {
						return err
					}
					inRange[i] = p.pop().boolean()
				}
				matched = inRange[i]
				if inRange[i] {
					err := p.execute(action.Pattern[1])
					if err != nil {
						return err
					}
					inRange[i] = !p.pop().boolean()
				}
			}
			if !matched {
				continue
			}

			// No action is equivalent to { print $0 }
			if len(action.Body) == 0 {
				err := p.printLine(p.output, p.line)
				if err != nil {
					return err
				}
				continue
			}

			// Execute the body statements
			err := p.execute(action.Body)
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

// Get a special variable by index
func (p *interp) getSpecial(index int) value {
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

// Set a variable by name (specials and globals only)
func (p *interp) setVarByName(name, value string) error {
	index := ast.SpecialVarIndex(name)
	if index > 0 {
		return p.setSpecial(index, numStr(value))
	}
	index, ok := p.program.Scalars[name]
	if ok {
		p.globals[index] = numStr(value)
		return nil
	}
	// Ignore variables that aren't defined in program
	return nil
}

// Set special variable by index to given value
func (p *interp) setSpecial(index int, v value) error {
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

// Determine the index of given array into the p.arrays slice. Global
// arrays are just at p.arrays[index], local arrays have to be looked
// up indirectly.
func (p *interp) arrayIndex(scope ast.VarScope, index int) int {
	if scope == ast.ScopeGlobal {
		return index
	} else {
		return p.localArrays[len(p.localArrays)-1][index]
	}
}

// Return array with given scope and index.
func (p *interp) array(scope ast.VarScope, index int) map[string]value {
	return p.arrays[p.arrayIndex(scope, index)]
}

// Return local array with given index.
func (p *interp) localArray(index int) map[string]value {
	return p.arrays[p.localArrays[len(p.localArrays)-1][index]]
}

// Set a value in given array by key (index)
func (p *interp) setArrayValue(scope ast.VarScope, arrayIndex int, index string, v value) {
	array := p.array(scope, arrayIndex)
	array[index] = v
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
