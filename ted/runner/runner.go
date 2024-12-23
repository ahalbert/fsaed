package runner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"

	"github.com/ahalbert/ted/ted/ast"
	"github.com/ahalbert/ted/ted/flags"
	"github.com/ahalbert/ted/ted/parser"
	"github.com/rwtodd/Go.Sed/sed"
)

type Runner struct {
	States                map[string]*State
	Variables             map[string]string
	Functions             map[string]*ast.FunctionLiteral
	StartState            string
	CurrState             string
	DidTransition         bool
	DidResetUnderscoreVar bool
	CaptureMode           string
	CaptureVar            string
	CurrLine              int
	MaxLine               int
	Tape                  io.ReadSeeker
	TapeOffsets           map[int]int64
	ShouldHalt            bool
}

type State struct {
	StateName string
	NextState string
	Actions   []ast.Action
}

func NewRunner(fsa ast.FSA, p *parser.Parser) *Runner {
	r := &Runner{
		States:      make(map[string]*State),
		Variables:   make(map[string]string),
		Functions:   make(map[string]*ast.FunctionLiteral),
		TapeOffsets: make(map[int]int64),
	}
	r.States["0"] = newState("0")
	r.Variables["$_"] = ""
	r.CurrLine = -1
	r.MaxLine = 0
	r.TapeOffsets[0] = 0

	for _, varstring := range flags.Flags.Variables {
		re, err := regexp.Compile("(.*?)=(.*)")
		if err != nil {
			panic("regex compile error")
		}
		matches := re.FindStringSubmatch(varstring)
		if matches != nil {
			r.Variables[matches[1]] = matches[2]
		} else {
			panic("unparsable variable --var " + varstring)
		}
	}

	for idx, statement := range fsa.Statements {
		switch statement.(type) {
		case *ast.StateStatement:
			r.processStateStatement(statement.(*ast.StateStatement), getNextStateInList(fsa.Statements, idx, statement.(*ast.StateStatement).StateName))
		case *ast.FunctionStatement:
			r.processFunctionStatement(statement.(*ast.FunctionStatement))
		}
	}
	return r
}

func getNextStateInList(statements []ast.Statement, idx int, currState string) string {
	if idx+1 >= len(statements) {
		return "0"
	}

	found := false
	for _, statement := range statements[idx:] {
		switch statement.(type) {
		case *ast.StateStatement:
			stmt := statement.(*ast.StateStatement)
			if stmt.StateName == currState {
				found = true
			} else if found {
				return stmt.StateName
			}
		}
	}
	return "0"
}

func (r *Runner) processStateStatement(statement *ast.StateStatement, nextState string) {
	if r.StartState == "" {
		r.StartState = statement.StateName
	}
	_, ok := r.States[statement.StateName]
	if !ok {
		r.States[statement.StateName] = newState(statement.StateName)
	}
	state, _ := r.States[statement.StateName]

	if state.NextState == "" {
		state.NextState = nextState
	}
	state.addRule(statement.Action)

}

func (r *Runner) processFunctionStatement(statement *ast.FunctionStatement) {
	switch statement.Function.(type) {
	case *ast.FunctionLiteral:
		fn := statement.Function.(*ast.FunctionLiteral)
		r.Functions[statement.Name] = fn
	default:
		panic("non-function expr")
	}

}

func newState(stateName string) *State {
	return &State{StateName: stateName,
		Actions:   []ast.Action{},
		NextState: "",
	}
}

func (s *State) addRule(action ast.Action) {
	s.Actions = append(s.Actions, action)
}

func (r *Runner) RunFSA(input io.ReadSeeker) {
	r.Tape = input
	if flags.Flags.NoPrint {
		r.CaptureMode = "capture"
		r.CaptureVar = "$NULL"
	} else {
		r.CaptureMode = "nocapture"
	}

	r.CurrState = r.StartState
	for !r.ShouldHalt {
		r.CurrLine++
		line, err := r.getLine(r.CurrLine)
		if err != nil && errors.Is(err, io.EOF) {
			r.ShouldHalt = true
			os.Exit(0)
		} else if err != nil {
			panic(err)
		}
		r.clearAndSetVariable("$@", line)

		if !(r.CaptureVar == "$_" && r.CaptureMode == "capture") {
			r.clearAndSetVariable("$_", r.getVariable("$@"))
			r.DidResetUnderscoreVar = true
		} else {
			r.DidResetUnderscoreVar = false
		}

		r.DidTransition = false

		state, ok := r.States[r.CurrState]
		if !ok {
			panic("missing state:" + r.CurrState)
		}

		for _, action := range state.Actions {
			if r.DidTransition {
				break
			}
			r.doAction(action)
		}

		if r.CaptureMode == "capture" {
			r.appendToVariable(r.CaptureVar, r.getVariable("$@")+"\n")
		} else if r.CaptureMode == "temp" {
			r.CaptureMode = "nocapture"
		} else if !flags.Flags.NoPrint {
			io.WriteString(os.Stdout, r.getVariable("$_")+"\n")
			r.clearAndSetVariable("$_", "")
		} else {
			r.clearAndSetVariable("$_", "")
		}
	}
}

func (r *Runner) getLine(line int) (string, error) {
	if line < 0 {
		return "", fmt.Errorf("line %d less than 0", line)
	}
	offset, ok := r.TapeOffsets[line]
	if !ok {
		linenum := r.MaxLine
		offset, _ := r.TapeOffsets[linenum]
		r.Tape.Seek(offset, 0)
		for linenum < line {
			_, err := r.readToNewline()
			linenum++
			if err != nil {
				return "", err
			}
			offset, err = r.Tape.Seek(0, io.SeekCurrent)
			if err != nil {
				panic(err)
			}
			r.TapeOffsets[linenum] = offset
			r.MaxLine = linenum
		}
	} else {
		r.Tape.Seek(offset, 0)
	}
	return r.readToNewline()
}

func (r *Runner) readToNewline() (string, error) {
	line := ""
	bit := make([]byte, 1)
	for ok := true; ok; ok = (bit[0] != '\n') {
		line += string(bit[0])
		_, err := r.Tape.Read(bit)
		if err != nil {
			return "", err
		}
	}
	return line[1:], nil
}

func (r *Runner) getVariable(key string) string {
	val, ok := r.Variables[key]
	if !ok {
		panic("Attempted to reference non-existent variable " + key)
	}
	return val
}

func (r *Runner) appendToVariable(key string, apendee string) string {
	if key == "$NULL" {
		return ""
	}
	val, ok := r.Variables[key]
	if !ok {
		val = ""
	}
	val = val + apendee
	r.Variables[key] = val
	return val
}

func (r *Runner) clearAndSetVariable(key string, toset string) {
	r.Variables[key] = toset
}

func (r *Runner) doTransition(newState string) {
	r.CurrState = newState
	r.DidTransition = true
}

func (r *Runner) applyVariablesToString(input string) string {
	var output bytes.Buffer
	t := template.Must(template.New("").Parse(input))
	t.Execute(&output, r.Variables)
	return output.String()
}

func (r *Runner) doAction(action ast.Action) {
	switch action.(type) {
	case *ast.ActionBlock:
		r.doActionBlock(action.(*ast.ActionBlock))
	case *ast.RegexAction:
		r.doRegexAction(action.(*ast.RegexAction))
	case *ast.DoSedAction:
		r.doSedAction(action.(*ast.DoSedAction))
	case *ast.GotoAction:
		r.doGotoAction(action.(*ast.GotoAction))
	case *ast.PrintAction:
		r.doPrintAction(action.(*ast.PrintAction))
	case *ast.PrintLnAction:
		r.doPrintLnAction(action.(*ast.PrintLnAction))
	case *ast.StartStopCaptureAction:
		r.doStartStopCapture(action.(*ast.StartStopCaptureAction))
	case *ast.CaptureAction:
		r.doCaptureAction(action.(*ast.CaptureAction))
	case *ast.ClearAction:
		r.doClearAction(action.(*ast.ClearAction))
	case *ast.AssignAction:
		r.doAssignAction(action.(*ast.AssignAction))
	case *ast.MoveHeadAction:
		r.doMoveHeadAction(action.(*ast.MoveHeadAction))
	case *ast.IfAction:
		r.doIfAction(action.(*ast.IfAction))
	case nil:
		r.doNoOp()
	default:
		panic("Unknown Action!")
	}
}

func (r *Runner) doActionBlock(block *ast.ActionBlock) {
	for _, action := range block.Actions {
		r.doAction(action)
	}
}

func (r *Runner) doRegexAction(action *ast.RegexAction) {
	rule := r.applyVariablesToString(action.Rule)
	re, err := regexp.Compile(rule)
	if err != nil {
		panic("regexp error, supplied: " + action.Rule + "\n formatted as: " + rule)
	}

	matches := re.FindStringSubmatch(r.getVariable("$@"))
	if matches != nil {
		for idx, match := range matches {
			stridx := "$" + strconv.Itoa(idx)
			r.clearAndSetVariable(stridx, match)
		}
		r.doAction(action.Action)
	}
}

func (r *Runner) doSedAction(action *ast.DoSedAction) {
	command := r.applyVariablesToString(action.Command)
	engine, err := sed.New(strings.NewReader(command))
	if err != nil {
		panic("error building sed engine with command: '" + action.Command + "'\n formatted as: '" + command + "'")
	}
	result, err := engine.RunString(r.getVariable(action.Variable))
	if err != nil {
		panic("error running sed")
	}
	if action.Variable == "$_" && r.CaptureMode != "capture" {
		r.clearAndSetVariable(action.Variable, result[:len(result)-1])
	} else {
		r.clearAndSetVariable(action.Variable, result)
	}
}

func (r *Runner) doGotoAction(action *ast.GotoAction) {
	if action.Target == "" {
		state, ok := r.States[r.CurrState]
		if !ok {
			panic(fmt.Sprintf("State %s not found", r.CurrState))
		}
		r.CurrState = state.NextState
	} else {
		r.CurrState = action.Target
	}
	r.DidTransition = true
}

func (r *Runner) doPrintAction(action *ast.PrintAction) {
	io.WriteString(os.Stdout, r.getVariable(action.Variable))
}

func (r *Runner) doPrintLnAction(action *ast.PrintLnAction) {
	io.WriteString(os.Stdout, r.getVariable(action.Variable)+"\n")
}

func (r *Runner) doCaptureAction(action *ast.CaptureAction) {
	if action.Variable == "$_" && r.DidResetUnderscoreVar {
		r.clearAndSetVariable("$_", "")
	}
	r.appendToVariable(action.Variable, r.getVariable("$@"))
	r.CaptureMode = "temp"
}

func (r *Runner) doStartStopCapture(action *ast.StartStopCaptureAction) {
	if action.Command == "start" {
		if action.Variable == "$_" {
			r.clearAndSetVariable("$_", "")
		}
		r.CaptureMode = "capture"
		r.CaptureVar = action.Variable
	} else if action.Command == "stop" {
		r.CaptureMode = "nocapture"
	} else {
		panic("unknown command: " + action.Command + " in start/stop action")
	}
}

func (r *Runner) doClearAction(action *ast.ClearAction) {
	if action.Variable == "$_" {
		r.CaptureMode = "nocapture"
	}
	r.clearAndSetVariable(action.Variable, "")
}

func (r *Runner) doAssignAction(action *ast.AssignAction) {
	val := r.evaluateExpression(action.Expression).String() //TODO: check if this is safe
	r.Variables[action.Target] = val
}

func (r *Runner) evaluateExpression(expression ast.Expression) ast.Expression {
	switch expression.(type) {
	case *ast.Boolean:
		return expression
	case *ast.IntegerLiteral:
		return expression
	case *ast.StringLiteral:
		return expression
	case *ast.Identifier:
		return &ast.StringLiteral{Value: r.getVariable(expression.(*ast.Identifier).Value)}
	case *ast.PrefixExpression:
		return r.evaluatePrefixExpression(expression.(*ast.PrefixExpression))
	case *ast.InfixExpression:
		return r.evaluateInfixExpression(expression.(*ast.InfixExpression))
	case *ast.CallExpression:
		return r.evaluateCallExpression(expression.(*ast.CallExpression))
	}
	return nil
}

func (r *Runner) evaluatePrefixExpression(expression *ast.PrefixExpression) ast.Expression {
	right := r.evaluateExpression(expression.Right)
	switch expression.Operator {
	case "!":
		switch right.(type) {
		case *ast.Boolean:
			return &ast.Boolean{Value: !right.(*ast.Boolean).Value}
		default:
			panic("! operation expects boolean.")
		}
	case "-":
		switch right.(type) {
		case *ast.IntegerLiteral:
			return &ast.IntegerLiteral{Value: -1 * right.(*ast.IntegerLiteral).Value}
		default:
			panic("- operation expects integer.")
		}
	}
	return nil
}

func (r *Runner) evaluateInfixExpression(expression *ast.InfixExpression) ast.Expression {
	left := r.evaluateExpression(expression.Left)
	right := r.evaluateExpression(expression.Right)
	if slices.Contains([]string{"+", "-", "*", "/"}, expression.Operator) {
		return r.evaluateArithmetic(left, right, expression.Operator)

	} else if slices.Contains([]string{">", "<", "!=", "=="}, expression.Operator) {
		result, err := r.tryCompareInt(left, right, expression.Operator)
		if err == nil {
			return result
		}
		result, err = r.tryCompareBool(left, right, expression.Operator)
		if err == nil {
			return result
		}
		result, err = r.tryCompareString(left, right, expression.Operator)
		if err == nil {
			return result
		}
		panic("unable to make comparison")
	}
	return nil
}

func (r *Runner) evaluateArithmetic(left ast.Expression, right ast.Expression, op string) ast.Expression {
	l_int, _ := r.convertToInt(left)
	r_int, r_err := r.convertToInt(right)
	switch op {
	case "+":
		return &ast.IntegerLiteral{Value: l_int + r_int}
	case "-":
		return &ast.IntegerLiteral{Value: l_int - r_int}
	case "*":
		return &ast.IntegerLiteral{Value: l_int * r_int}
	case "/":
		if r_err != nil {
			return &ast.IntegerLiteral{Value: 0}
		}
		return &ast.IntegerLiteral{Value: l_int / r_int}
	}
	return nil
}

func (r *Runner) convertToInt(expression ast.Expression) (int, error) {
	switch expression.(type) {
	case *ast.StringLiteral:
		val, err := strconv.Atoi(expression.(*ast.StringLiteral).Value)
		if err != nil {
			return 0, fmt.Errorf("type error expected int or string-like int")
		}
		return val, nil
	case *ast.IntegerLiteral:
		return expression.(*ast.IntegerLiteral).Value, nil
	default:
		return 0, fmt.Errorf("type error expected int or string-like int")
	}
}

func (r *Runner) tryCompareInt(left ast.Expression, right ast.Expression, op string) (ast.Expression, error) {
	l_int, l_err := r.convertToInt(left)
	r_int, r_err := r.convertToInt(right)
	if l_err != nil || r_err != nil {
		return nil, fmt.Errorf("unable to convert to int")
	}
	switch op {
	case ">":
		return &ast.Boolean{Value: l_int > r_int}, nil
	case "<":
		return &ast.Boolean{Value: l_int < r_int}, nil
	case "==":
		return &ast.Boolean{Value: l_int == r_int}, nil
	case "!=":
		return &ast.Boolean{Value: l_int != r_int}, nil
	}
	return nil, fmt.Errorf("unknown operator")
}

func (r *Runner) convertToBool(expression ast.Expression) (bool, error) {
	switch expression.(type) {
	case *ast.StringLiteral:
		val := expression.(*ast.StringLiteral).Value
		if val == "true" {
			return true, nil
		}
		if val == "false" {
			return false, nil
		}
		return false, fmt.Errorf("unable to convert to bool.")
	case *ast.Boolean:
		return expression.(*ast.Boolean).Value, nil
	default:
		return false, fmt.Errorf("type error expected bool or string-like bool")
	}
}

func (r *Runner) tryCompareBool(left ast.Expression, right ast.Expression, op string) (ast.Expression, error) {
	lbool, l_err := r.convertToBool(left)
	rbool, r_err := r.convertToBool(right)
	if l_err != nil || r_err != nil {
		return nil, fmt.Errorf("unable to convert to int")
	}
	switch op {
	case ">":
		return &ast.Boolean{Value: false}, fmt.Errorf("> not compatible with bool compare")
	case "<":
		return &ast.Boolean{Value: false}, fmt.Errorf("< not compatible with bool compare")
	case "==":
		return &ast.Boolean{Value: lbool == rbool}, nil
	case "!=":
		return &ast.Boolean{Value: lbool != rbool}, nil
	}
	return nil, fmt.Errorf("unknown operator")
}

func (r *Runner) tryCompareString(left ast.Expression, right ast.Expression, op string) (ast.Expression, error) {
	leftStr := left.(*ast.StringLiteral).Value
	rightStr := right.(*ast.StringLiteral).Value
	switch op {
	case ">":
		return &ast.Boolean{Value: leftStr > rightStr}, nil
	case "<":
		return &ast.Boolean{Value: leftStr < rightStr}, nil
	case "==":
		return &ast.Boolean{Value: leftStr == rightStr}, nil
	case "!=":
		return &ast.Boolean{Value: leftStr != rightStr}, nil
	}
	return nil, fmt.Errorf("unknown operator")
}

func (r *Runner) evaluateCallExpression(expression *ast.CallExpression) ast.Expression {
	return expression
}

func (r *Runner) doMoveHeadAction(action *ast.MoveHeadAction) {
	if action.Command == "fastforward" {
		r.doFastForward(action.Regex)
	} else if action.Command == "rewind" {
		r.doRewind(action.Regex)
	}
}

func (r *Runner) doFastForward(target string) {
	rule := r.applyVariablesToString(target)
	re, err := regexp.Compile(rule)
	if err != nil {
		panic(err)
	}
	line := ""
	for ok := true; ok; ok = (!re.MatchString(line)) {
		r.CurrLine++
		line, err = r.getLine(r.CurrLine)
		if err != nil && errors.Is(err, io.EOF) {
			r.ShouldHalt = true
			os.Exit(0)
		} else if err != nil {
			panic(err)
		}
	}
	r.CurrLine--
}

func (r *Runner) doRewind(target string) {
	rule := r.applyVariablesToString(target)
	re, err := regexp.Compile(rule)
	if err != nil {
		panic(err)
	}
	line := ""
	for ok := true; ok; ok = (!re.MatchString(line) && r.CurrLine >= 0) {
		r.CurrLine--
		line, err = r.getLine(r.CurrLine)
		if err != nil {
			panic(err)
		}
	}
	r.CurrLine--
}

func (r *Runner) doIfAction(action *ast.IfAction) {
	exprResult := false
	result := r.evaluateExpression(action.Condition)
	switch result.(type) {
	case *ast.Boolean:
		exprResult = result.(*ast.Boolean).Value
	default:
		panic("type error expected bool in if statement")
	}
	if exprResult {
		r.doAction(action.Consequence)
	} else if action.Alternative != nil {
		r.doAction(action.Alternative)
	}
}

func (r *Runner) doNoOp() {}
