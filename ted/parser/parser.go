package parser

import (
	"fmt"
	"strconv"

	"github.com/ahalbert/ted/ted/ast"
	"github.com/ahalbert/ted/ted/lexer"
	"github.com/ahalbert/ted/ted/token"
)

const (
	_ int = iota
	LOWEST
	EQUALS      // ==
	LESSGREATER // > or <
	SUM         // +
	PRODUCT     // *
	PREFIX      // -X or !X
	CALL        // myFunction(X)
)

var precedences = map[token.TokenType]int{
	token.EQ:       EQUALS,
	token.NOT_EQ:   EQUALS,
	token.LT:       LESSGREATER,
	token.GT:       LESSGREATER,
	token.PLUS:     SUM,
	token.MINUS:    SUM,
	token.SLASH:    PRODUCT,
	token.ASTERISK: PRODUCT,
	token.LPAREN:   CALL,
}

type (
	prefixParseFn func() ast.Expression
	infixParseFn  func(ast.Expression) ast.Expression
)

type Parser struct {
	l      *lexer.Lexer
	errors []string

	curToken       token.Token
	peekToken      token.Token
	prefixParseFns map[token.TokenType]prefixParseFn
	infixParseFns  map[token.TokenType]infixParseFn

	AnonymousStates                int
	GOTOsThatNeedNextStateAssigned []*ast.GotoAction
}

func New(l *lexer.Lexer) *Parser {
	p := &Parser{
		l:                              l,
		errors:                         []string{},
		AnonymousStates:                1,
		GOTOsThatNeedNextStateAssigned: []*ast.GotoAction{},
	}

	p.prefixParseFns = make(map[token.TokenType]prefixParseFn)
	p.infixParseFns = make(map[token.TokenType]infixParseFn)
	p.registerPrefix(token.IDENT, p.parseIdentifierExpr)
	p.registerPrefix(token.STRING, p.parseStringLiteralExpr)

	p.registerPrefix(token.BANG, p.parsePrefixExpression)
	p.registerPrefix(token.MINUS, p.parsePrefixExpression)
	p.registerPrefix(token.LPAREN, p.parseGroupedExpression)
	p.registerPrefix(token.FUNCTION, p.parseFunctionLiteral)

	p.registerInfix(token.PLUS, p.parseInfixExpression)
	p.registerInfix(token.MINUS, p.parseInfixExpression)
	p.registerInfix(token.SLASH, p.parseInfixExpression)
	p.registerInfix(token.ASTERISK, p.parseInfixExpression)
	p.registerInfix(token.EQ, p.parseInfixExpression)
	p.registerInfix(token.NOT_EQ, p.parseInfixExpression)
	p.registerInfix(token.LT, p.parseInfixExpression)
	p.registerInfix(token.GT, p.parseInfixExpression)

	p.registerInfix(token.LPAREN, p.parseCallExpression)
	// Read two tokens, so curToken and peekToken are both set
	p.nextToken()
	p.nextToken()

	return p
}

func (p *Parser) registerPrefix(tokenType token.TokenType, fn prefixParseFn) {
	p.prefixParseFns[tokenType] = fn
}

func (p *Parser) registerInfix(tokenType token.TokenType, fn infixParseFn) {
	p.infixParseFns[tokenType] = fn
}

func (p *Parser) nextToken() {
	p.curToken = p.peekToken
	p.peekToken = p.l.NextToken()
}

func (p *Parser) curTokenIs(t token.TokenType) bool {
	return p.curToken.Type == t
}

func (p *Parser) peekTokenIs(t token.TokenType) bool {
	return p.peekToken.Type == t
}

func (p *Parser) curPrecedence() int {
	if precedence, ok := precedences[p.curToken.Type]; ok {
		return precedence
	}
	return LOWEST
}

func (p *Parser) peekPrecedence() int {
	if precedence, ok := precedences[p.peekToken.Type]; ok {
		return precedence
	}
	return LOWEST
}

func (p *Parser) expectPeek(t token.TokenType) bool {
	if p.peekTokenIs(t) {
		p.nextToken()
		return true
	} else {
		p.peekError(t)
		return false
	}
}

func (p *Parser) peekError(t token.TokenType) {
	msg := fmt.Sprintf("expected next token to be %s, got %s instead",
		t, p.peekToken.Type)
	panic(msg)
	// p.errors = append(p.errors, msg)
}

func (p *Parser) noPrefixParseFnError(t token.TokenType) {
	msg := fmt.Sprintf("no prefix parse function for %s found", t)
	panic(msg)
	//p.errors = append(p.errors, msg)
}

func (p *Parser) ParseFSA() ast.FSA {
	program := ast.FSA{}
	program.Statements = []ast.Statement{}

	for !p.curTokenIs(token.EOF) {
		stmt := p.parseStatement()
		program.Statements = append(program.Statements, stmt)
	}

	return program
}

func (p *Parser) parseStatement() ast.Statement {
	statement := &ast.StateStatement{}
	if p.curTokenIs(token.LABEL) {
		statement.StateName = p.curToken.Literal
		p.nextToken()
	} else if p.curTokenIs(token.FUNCTION) {
		return p.parseFunctionStatement()
	} else {
		statement.StateName = strconv.Itoa(p.AnonymousStates)
		p.AnonymousStates++
	}

	statement.Action = p.parseAction()
	return statement
}

func (p *Parser) parseFunctionStatement() *ast.FunctionStatement {
	function := &ast.FunctionStatement{}
	p.nextToken()
	if !p.curTokenIs(token.IDENT) {
		panic("expected function identifier")
	}
	function.Name = p.curToken.Literal
	p.nextToken()
	function.Function = p.parseFunctionLiteral()
	return function
}

func (p *Parser) parseAction() ast.Action {
	var action ast.Action
	switch p.curToken.Type {
	case token.LBRACE:
		action = p.parseActionBlock()
	case token.REGEX:
		action = p.parseRegexAction()
	case token.GOTO:
		action = p.parseGotoAction()
	case token.RESET:
		action = p.parseGotoAction()
	case token.DO:
		action = p.parseDoAction()
	case token.PRINT:
		action = p.parsePrintAction()
	case token.PRINTLN:
		action = p.parsePrintLnAction()
	case token.START:
		action = p.parseStartStopCaptureAction()
	case token.STOP:
		action = p.parseStartStopCaptureAction()
	case token.CAPTURE:
		action = p.parseCaptureAction()
	case token.LET:
		action = p.parseAssignAction()
	case token.REWIND:
		action = p.parseMoveHeadAction()
	case token.FASTFWD:
		action = p.parseMoveHeadAction()
	case token.IF:
		action = p.parseIfAction()
	default:
		panic("parser error: expected action")
	}
	return action
}

func (p *Parser) parseActionBlock() *ast.ActionBlock {
	action := &ast.ActionBlock{}
	p.nextToken()
	for !p.curTokenIs(token.RBRACE) {
		action.Actions = append(action.Actions, p.parseAction())
	}
	p.nextToken()
	return action
}

func (p *Parser) parseRegexAction() *ast.RegexAction {
	action := &ast.RegexAction{Rule: p.curToken.Literal}
	p.nextToken()
	action.Action = p.parseAction()
	return action
}

func (p *Parser) parseGotoAction() *ast.GotoAction {
	action := &ast.GotoAction{}
	if p.peekTokenIs(token.IDENT) {
		p.nextToken()
		action.Target = p.curToken.Literal
	}

	p.nextToken()
	return action
}

func (p *Parser) parseDoAction() *ast.DoSedAction {
	action := &ast.DoSedAction{Command: p.curToken.Literal}
	action.Variable = p.helpCheckForOptionalVarArg()
	return action
}

func (p *Parser) parsePrintAction() *ast.PrintAction {
	action := &ast.PrintAction{}
	action.Variable = p.helpCheckForOptionalVarArg()
	return action
}

func (p *Parser) parsePrintLnAction() *ast.PrintLnAction {
	action := &ast.PrintLnAction{}
	action.Variable = p.helpCheckForOptionalVarArg()
	return action
}

func (p *Parser) parseClearAction() *ast.ClearAction {
	action := &ast.ClearAction{}
	action.Variable = p.helpCheckForOptionalVarArg()
	return action
}

func (p *Parser) parseStartStopCaptureAction() *ast.StartStopCaptureAction {
	action := &ast.StartStopCaptureAction{Command: p.curToken.Literal}
	p.nextToken()
	if p.curTokenIs(token.CAPTURE) {
		action.Variable = p.helpCheckForOptionalVarArg()
	} else {
		panic("parser error: expected keyword capture")
	}
	return action
}

func (p *Parser) helpCheckForOptionalVarArg() string {
	p.nextToken()
	if p.curTokenIs(token.IDENT) {
		variable := p.curToken.Literal
		p.nextToken()
		return variable
	} else {
		return "$_"
	}
}

func (p *Parser) parseCaptureAction() *ast.CaptureAction {
	action := &ast.CaptureAction{}
	action.Variable = p.helpCheckForOptionalVarArg()
	return action
}

func (p *Parser) parseAssignAction() *ast.AssignAction {
	action := &ast.AssignAction{}
	p.nextToken()
	if p.curTokenIs(token.IDENT) {
		//TODO: Check is valid variable
		action.Target = p.curToken.Literal
	} else {
		panic("parser error: expected identifier")
	}

	p.nextToken()
	if !p.curTokenIs(token.ASSIGN) {
		panic("parser error: expected =")
	}

	p.nextToken()
	action.Expression = p.parseExpression(LOWEST)
	p.nextToken()

	return action
}

func (p *Parser) parseIfAction() *ast.IfAction {
	action := &ast.IfAction{}
	p.nextToken()
	action.Condition = p.parseExpression(LOWEST)

	p.nextToken()
	action.Consequence = p.parseAction()

	if p.curTokenIs(token.ELSE) {
		p.nextToken()
		action.Alternative = p.parseAction()
	}
	return action
}

func (p *Parser) parseMoveHeadAction() *ast.MoveHeadAction {
	t := p.curToken.Type
	action := &ast.MoveHeadAction{Command: p.curToken.Literal}
	p.nextToken()
	if t == token.REWIND || t == token.FASTFWD {
		if p.curTokenIs(token.REGEX) {
			action.Regex = p.curToken.Literal
			p.nextToken()
		} else {
			panic("expected regex with " + action.Command)
		}
	}
	return action
}

func (p *Parser) parseExpression(precedence int) ast.Expression {
	prefix := p.prefixParseFns[p.curToken.Type]
	if prefix == nil {
		//p.noPrefixParseFnError(p.curToken.Type)
		return nil
	}
	leftExp := prefix()
	for precedence < p.peekPrecedence() {
		infix := p.infixParseFns[p.peekToken.Type]
		if infix == nil {
			return leftExp
		}
		p.nextToken()
		leftExp = infix(leftExp)
	}

	return leftExp
}

func (p *Parser) parseIdentifierExpr() ast.Expression {
	val, err := strconv.Atoi(p.curToken.Literal)
	if err == nil {
		return &ast.IntegerLiteral{Value: val}
	}
	if p.curToken.Literal == "false" {
		return &ast.Boolean{Value: false}
	}
	if p.curToken.Literal == "true" {
		return &ast.Boolean{Value: true}
	}
	return &ast.Identifier{Value: p.curToken.Literal}
}

func (p *Parser) parseStringLiteralExpr() ast.Expression {
	lit := &ast.StringLiteral{Value: p.curToken.Literal}
	p.nextToken()
	return lit
}

func (p *Parser) parsePrefixExpression() ast.Expression {
	expression := &ast.PrefixExpression{Operator: p.curToken.Literal}
	p.nextToken()
	expression.Right = p.parseExpression(PREFIX)
	return expression
}

func (p *Parser) parseGroupedExpression() ast.Expression {
	p.nextToken()
	exp := p.parseExpression(LOWEST)
	if !p.expectPeek(token.RPAREN) {
		return nil
	}
	return exp
}

func (p *Parser) parseFunctionLiteral() ast.Expression {
	lit := &ast.FunctionLiteral{}

	if !p.curTokenIs(token.LPAREN) {
		panic("expected (")
	}

	lit.Parameters = p.parseFunctionParameters()

	if !p.expectPeek(token.LBRACE) {
		return nil
	}

	lit.Body = p.parseAction()

	return lit
}

func (p *Parser) parseFunctionParameters() []*ast.Identifier {
	identifiers := []*ast.Identifier{}

	if p.peekTokenIs(token.RPAREN) {
		p.nextToken()
		return identifiers
	}

	p.nextToken()

	ident := &ast.Identifier{Value: p.curToken.Literal}
	identifiers = append(identifiers, ident)

	for p.peekTokenIs(token.COMMA) {
		p.nextToken()
		p.nextToken()
		ident := &ast.Identifier{Value: p.curToken.Literal}
		identifiers = append(identifiers, ident)
	}

	if !p.expectPeek(token.RPAREN) {
		return nil
	}

	return identifiers
}

func (p *Parser) parseInfixExpression(left ast.Expression) ast.Expression {
	expression := &ast.InfixExpression{
		Operator: p.curToken.Literal,
		Left:     left,
	}

	precedence := p.curPrecedence()
	p.nextToken()
	expression.Right = p.parseExpression(precedence)

	return expression
}

func (p *Parser) parseCallExpression(function ast.Expression) ast.Expression {
	exp := &ast.CallExpression{Function: function}
	exp.Arguments = p.parseCallArguments()
	return exp
}

func (p *Parser) parseCallArguments() []ast.Expression {
	args := []ast.Expression{}

	if p.peekTokenIs(token.RPAREN) {
		p.nextToken()
		return args
	}

	p.nextToken()
	args = append(args, p.parseExpression(LOWEST))

	for p.peekTokenIs(token.COMMA) {
		p.nextToken()
		p.nextToken()
		args = append(args, p.parseExpression(LOWEST))
	}

	if !p.expectPeek(token.RPAREN) {
		return nil
	}

	return args
}
