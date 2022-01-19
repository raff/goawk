package compiler

import (
	"fmt"
	"io"
	"strings"

	"github.com/benhoyt/goawk/internal/ast"
	"github.com/benhoyt/goawk/lexer"
)

func (p *Program) Disassemble(writer io.Writer) error {
	if p.Begin != nil {
		d := &disassembler{
			program:         p,
			writer:          writer,
			code:            p.Begin,
			nativeFuncNames: p.nativeFuncNames,
		}
		err := d.disassemble("BEGIN")
		if err != nil {
			return err
		}
	}

	for _, action := range p.Actions {
		switch len(action.Pattern) {
		case 0:
		case 1:
			d := &disassembler{
				program:         p,
				writer:          writer,
				code:            action.Pattern[0],
				nativeFuncNames: p.nativeFuncNames,
			}
			err := d.disassemble("pattern")
			if err != nil {
				return err
			}
		case 2:
			d := &disassembler{
				program:         p,
				writer:          writer,
				code:            action.Pattern[0],
				nativeFuncNames: p.nativeFuncNames,
			}
			err := d.disassemble("start")
			if err != nil {
				return err
			}
			d = &disassembler{
				program:         p,
				writer:          writer,
				code:            action.Pattern[1],
				nativeFuncNames: p.nativeFuncNames,
			}
			err = d.disassemble("stop")
			if err != nil {
				return err
			}
		}
		if len(action.Body) > 0 {
			d := &disassembler{
				program:         p,
				writer:          writer,
				code:            action.Body,
				nativeFuncNames: p.nativeFuncNames,
			}
			err := d.disassemble("{ body }")
			if err != nil {
				return err
			}
		}
	}

	if p.End != nil {
		d := &disassembler{
			program:         p,
			writer:          writer,
			code:            p.End,
			nativeFuncNames: p.nativeFuncNames,
		}
		err := d.disassemble("END")
		if err != nil {
			return err
		}
	}

	for i, f := range p.Functions {
		d := &disassembler{
			program:         p,
			writer:          writer,
			code:            f.Body,
			nativeFuncNames: p.nativeFuncNames,
			funcIndex:       i,
		}
		err := d.disassemble("function " + f.Name)
		if err != nil {
			return err
		}
	}

	return nil
}

type disassembler struct {
	program         *Program
	writer          io.Writer
	code            []Opcode
	nativeFuncNames []string
	funcIndex       int
	ip              int
	opAddr          int
	err             error
}

func (d *disassembler) disassemble(prefix string) error {
	if prefix != "" {
		d.writef("        // %s\n", prefix)
	}

	for d.ip < len(d.code) && d.err == nil {
		d.opAddr = d.ip
		op := d.fetch()

		switch op {
		case Num:
			index := d.fetch()
			num := d.program.Nums[index]
			if num == float64(int(num)) {
				d.writeOpf("Num %d", int(num))
			} else {
				d.writeOpf("Num %.6g", num)
			}

		case Str:
			index := d.fetch()
			d.writeOpf("Str %q", d.program.Strs[index])

		case FieldNum:
			index := d.fetch()
			d.writeOpf("FieldNum %d", index)

		case Global:
			index := d.fetch()
			d.writeOpf("Global %s", d.program.scalarNames[index])

		case Local:
			index := int(d.fetch())
			d.writeOpf("Local %s", d.localName(index))

		case Special:
			index := d.fetch()
			d.writeOpf("Special %s", ast.SpecialVarName(int(index)))

		case ArrayGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("ArrayGlobal %s", d.program.arrayNames[arrayIndex])

		case ArrayLocal:
			arrayIndex := d.fetch()
			d.writeOpf("ArrayLocal %s", d.localArrayName(int(arrayIndex)))

		case InGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("InGlobal %s", d.program.arrayNames[arrayIndex])

		case InLocal:
			arrayIndex := int(d.fetch())
			d.writeOpf("InLocal %s", d.localArrayName(arrayIndex))

		case AssignGlobal:
			index := d.fetch()
			d.writeOpf("AssignGlobal %s", d.program.scalarNames[index])

		case AssignLocal:
			index := int(d.fetch())
			d.writeOpf("AssignLocal %s", d.localName(index))

		case AssignSpecial:
			index := d.fetch()
			d.writeOpf("AssignSpecial %s", ast.SpecialVarName(int(index)))

		case AssignArrayGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("AssignArrayGlobal %s", d.program.arrayNames[arrayIndex])

		case AssignArrayLocal:
			arrayIndex := int(d.fetch())
			d.writeOpf("AssignArrayLocal %s", d.localArrayName(arrayIndex))

		case DeleteGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("DeleteGlobal %s", d.program.arrayNames[arrayIndex])

		case DeleteLocal:
			arrayIndex := int(d.fetch())
			d.writeOpf("DeleteLocal %d", d.localArrayName(arrayIndex))

		case DeleteAllGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("DeleteAllGlobal %s", d.program.arrayNames[arrayIndex])

		case DeleteAllLocal:
			arrayIndex := int(d.fetch())
			d.writeOpf("DeleteAllLocal %d", d.localArrayName(arrayIndex))

		case IncrField:
			amount := int32(d.fetch())
			d.writeOpf("IncrField %d", amount)

		case IncrGlobal:
			amount := int32(d.fetch())
			index := d.fetch()
			d.writeOpf("IncrGlobal %d %s", amount, d.program.scalarNames[index])

		case IncrLocal:
			amount := int32(d.fetch())
			index := int(d.fetch())
			d.writeOpf("IncrLocal %d %s", amount, d.localName(index))

		case IncrSpecial:
			amount := int32(d.fetch())
			index := d.fetch()
			d.writeOpf("IncrSpecial %d %s", amount, ast.SpecialVarName(int(index)))

		case IncrArrayGlobal:
			amount := int32(d.fetch())
			arrayIndex := d.fetch()
			d.writeOpf("IncrArrayGlobal %d %s", amount, d.program.arrayNames[arrayIndex])

		case IncrArrayLocal:
			amount := int32(d.fetch())
			arrayIndex := int(d.fetch())
			d.writeOpf("IncrArrayLocal %d %s", amount, d.localArrayName(arrayIndex))

		case AugAssignField:
			operation := lexer.Token(d.fetch())
			d.writeOpf("AugAssignField %s", operation)

		case AugAssignGlobal:
			operation := lexer.Token(d.fetch())
			index := d.fetch()
			d.writeOpf("AugAssignGlobal %s %s", operation, d.program.scalarNames[index])

		case AugAssignLocal:
			operation := lexer.Token(d.fetch())
			index := int(d.fetch())
			d.writeOpf("AugAssignLocal %s %s", operation, d.localName(index))

		case AugAssignSpecial:
			operation := lexer.Token(d.fetch())
			index := d.fetch()
			d.writeOpf("AugAssignSpecial %s %d", operation, ast.SpecialVarName(int(index)))

		case AugAssignArrayGlobal:
			operation := lexer.Token(d.fetch())
			arrayIndex := d.fetch()
			d.writeOpf("AugAssignArrayGlobal %s %s", operation, d.program.arrayNames[arrayIndex])

		case AugAssignArrayLocal:
			operation := lexer.Token(d.fetch())
			arrayIndex := int(d.fetch())
			d.writeOpf("AugAssignArrayLocal %s %s", operation, d.localArrayName(arrayIndex))

		case Regex:
			regexIndex := d.fetch()
			d.writeOpf("Regex %q", d.program.Regexes[regexIndex])

		case MultiIndex:
			num := d.fetch()
			d.writeOpf("MultiIndex %d", num)

		case Jump:
			offset := int32(d.fetch())
			d.writeOpf("Jump 0x%04x", d.ip+int(offset))

		case JumpFalse:
			offset := int32(d.fetch())
			d.writeOpf("JumpFalse 0x%04x", d.ip+int(offset))

		case JumpTrue:
			offset := int32(d.fetch())
			d.writeOpf("JumpTrue 0x%04x", d.ip+int(offset))

		case JumpEquals:
			offset := int32(d.fetch())
			d.writeOpf("JumpEquals 0x%04x", d.ip+int(offset))

		case JumpNotEquals:
			offset := int32(d.fetch())
			d.writeOpf("JumpNotEquals 0x%04x", d.ip+int(offset))

		case JumpLess:
			offset := int32(d.fetch())
			d.writeOpf("JumpLess 0x%04x", d.ip+int(offset))

		case JumpGreater:
			offset := int32(d.fetch())
			d.writeOpf("JumpGreater 0x%04x", d.ip+int(offset))

		case JumpLessOrEqual:
			offset := int32(d.fetch())
			d.writeOpf("JumpLessOrEqual 0x%04x", d.ip+int(offset))

		case JumpGreaterOrEqual:
			offset := int32(d.fetch())
			d.writeOpf("JumpGreaterOrEqual 0x%04x", d.ip+int(offset))

		case ForInGlobal:
			// TODO: factor next three into function - also other similar opcodes?
			varIndex := d.fetch()
			arrayScope := d.fetch()
			arrayIndex := d.fetch()
			offset := d.fetch()
			var arrayName string
			if ast.VarScope(arrayScope) == ast.ScopeGlobal {
				arrayName = d.program.arrayNames[arrayIndex]
			} else {
				arrayName = d.localArrayName(int(arrayIndex))
			}
			d.writeOpf("ForInGlobal %s %s 0x%04x", d.program.scalarNames[varIndex], arrayName, d.ip+int(offset))

		case ForInLocal:
			varIndex := d.fetch()
			arrayScope := d.fetch()
			arrayIndex := d.fetch()
			offset := d.fetch()
			var arrayName string
			if ast.VarScope(arrayScope) == ast.ScopeGlobal {
				arrayName = d.program.arrayNames[arrayIndex]
			} else {
				arrayName = d.localArrayName(int(arrayIndex))
			}
			d.writeOpf("ForInLocal %s %s 0x%04x", d.localName(int(varIndex)), arrayName, d.ip+int(offset))

		case ForInSpecial:
			varIndex := d.fetch()
			arrayScope := d.fetch()
			arrayIndex := d.fetch()
			offset := d.fetch()
			var arrayName string
			if ast.VarScope(arrayScope) == ast.ScopeGlobal {
				arrayName = d.program.arrayNames[arrayIndex]
			} else {
				arrayName = d.localArrayName(int(arrayIndex))
			}
			d.writeOpf("ForInSpecial %s %s 0x%04x", ast.SpecialVarName(int(varIndex)), arrayName, d.ip+int(offset))

		//case CallGsub:
		//	d.writeOpf("CallGsub")
		//case CallGsubField:
		//	d.writeOpf("CallGsubField")
		//case CallGsubGlobal:
		//	d.writeOpf("CallGsubGlobal")
		//case CallGsubLocal:
		//	d.writeOpf("CallGsubLocal")
		//case CallGsubSpecial:
		//	d.writeOpf("CallGsubSpecial")
		//case CallGsubArrayGlobal:
		//	d.writeOpf("CallGsubArrayGlobal")
		//case CallGsubArrayLocal:
		//	d.writeOpf("CallGsubArrayLocal")

		case CallSplitGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("CallSplitGlobal %s", d.program.arrayNames[arrayIndex])

		case CallSplitLocal:
			arrayIndex := int(d.fetch())
			d.writeOpf("CallSplitLocal %s", d.localArrayName(arrayIndex))

		case CallSplitSepGlobal:
			arrayIndex := d.fetch()
			d.writeOpf("CallSplitSepGlobal %s", d.program.arrayNames[arrayIndex])

		case CallSplitSepLocal:
			arrayIndex := int(d.fetch())
			d.writeOpf("CallSplitSepLocal %s", d.localArrayName(arrayIndex))

		case CallSprintf:
			numArgs := d.fetch()
			d.writeOpf("CallSprintf %d", numArgs)

		//case CallSub:
		//	d.writeOpf("CallSub")
		//case CallSubField:
		//	d.writeOpf("CallSubField")
		//case CallSubGlobal:
		//	d.writeOpf("CallSubGlobal")
		//case CallSubLocal:
		//	d.writeOpf("CallSubLocal")
		//case CallSubSpecial:
		//	d.writeOpf("CallSubSpecial")
		//case CallSubArrayGlobal:
		//	d.writeOpf("CallSubArrayGlobal")
		//case CallSubArrayLocal:
		//	d.writeOpf("CallSubArrayLocal")

		case CallUser:
			funcIndex := d.fetch()
			numArrayArgs := int(d.fetch())
			var arrayArgs []string
			for i := 0; i < numArrayArgs; i++ {
				arrayScope := ast.VarScope(d.fetch())
				arrayIndex := int(d.fetch())
				switch arrayScope {
				case ast.ScopeGlobal:
					arrayArgs = append(arrayArgs, d.program.arrayNames[arrayIndex])
				case ast.ScopeLocal:
					arrayArgs = append(arrayArgs, d.localArrayName(arrayIndex))
				}
			}
			d.writeOpf("CallUser %s [%s]", d.program.Functions[funcIndex].Name, strings.Join(arrayArgs, ", "))

		case CallNative:
			funcIndex := d.fetch()
			numArgs := d.fetch()
			d.writeOpf("CallNative %s %d", d.nativeFuncNames[funcIndex], numArgs)

		case Nulls:
			numNulls := d.fetch()
			d.writeOpf("Nulls %d", numNulls)

		case Print:
			numArgs := d.fetch()
			redirect := lexer.Token(d.fetch())
			if redirect == lexer.ILLEGAL {
				d.writeOpf("Print %d", numArgs)
			} else {
				d.writeOpf("Print %d %s", numArgs, redirect)
			}

		case Printf:
			numArgs := d.fetch()
			redirect := lexer.Token(d.fetch())
			if redirect == lexer.ILLEGAL {
				d.writeOpf("Printf %d", numArgs)
			} else {
				d.writeOpf("Printf %d %s", numArgs, redirect)
			}

		case Getline:
			redirect := lexer.Token(d.fetch())
			d.writeOpf("Getline %s", redirect)

		case GetlineField:
			redirect := lexer.Token(d.fetch())
			d.writeOpf("GetlineField %s", redirect)

		case GetlineGlobal:
			redirect := lexer.Token(d.fetch())
			index := d.fetch()
			d.writeOpf("GetlineGlobal %s %s", redirect, d.program.scalarNames[index])

		case GetlineLocal:
			redirect := lexer.Token(d.fetch())
			index := int(d.fetch())
			d.writeOpf("GetlineLocal %s %s", redirect, d.localName(index))

		case GetlineSpecial:
			redirect := lexer.Token(d.fetch())
			index := d.fetch()
			d.writeOpf("GetlineSpecial %s %s", redirect, ast.SpecialVarName(int(index)))

		case GetlineArrayGlobal:
			redirect := lexer.Token(d.fetch())
			arrayIndex := d.fetch()
			d.writeOpf("GetlineArrayGlobal %s %s", redirect, d.program.arrayNames[arrayIndex])

		case GetlineArrayLocal:
			redirect := lexer.Token(d.fetch())
			arrayIndex := int(d.fetch())
			d.writeOpf("GetlineArrayLocal %s %s", redirect, d.localArrayName(arrayIndex))

		default:
			d.writeOpf("%s", op)
		}
	}

	d.writef("\n")
	return d.err
}

func (d *disassembler) fetch() Opcode {
	op := d.code[d.ip]
	d.ip++
	return op
}

func (d *disassembler) writef(format string, args ...interface{}) {
	if d.err != nil {
		return
	}
	_, d.err = fmt.Fprintf(d.writer, format, args...)
}

func (d *disassembler) writeOpf(format string, args ...interface{}) {
	if d.err != nil {
		return
	}
	addrStr := fmt.Sprintf("%04x", d.opAddr)
	_, d.err = fmt.Fprintf(d.writer, addrStr+"    "+format+"\n", args...)
}

func (d *disassembler) localName(index int) string {
	f := d.program.Functions[d.funcIndex]
	n := 0
	for i, p := range f.Params {
		if f.Arrays[i] {
			continue
		}
		if n == index {
			return p
		}
		n++
	}
	panic(fmt.Sprintf("unexpected local variable index %d", index))
}

func (d *disassembler) localArrayName(index int) string {
	f := d.program.Functions[d.funcIndex]
	n := 0
	for i, p := range f.Params {
		if !f.Arrays[i] {
			continue
		}
		if n == index {
			return p
		}
		n++
	}
	panic(fmt.Sprintf("unexpected local array index %d", index))
}