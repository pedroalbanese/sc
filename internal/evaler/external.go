// Package evaler — external.go
//
// This file implements the @evaler(filename, expr) function, which loads
// an external .sc or .csv file and evaluates an expression against it.
//
// The external file is loaded into a map of ExternalCell, and a
// self-contained evaluator (externalEvaluator) is used to evaluate the
// expression, so the external file's formulas are resolved in its own
// context (not the current sheet's).

package evaler

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ===========================================================================
// Types
// ===========================================================================

// ExternalCell represents a cell in an external spreadsheet.
type ExternalCell struct {
	Kind  int    // 0 = number, 1 = formula, 2 = string
	Addr  string
	Value string
}

// ExternalLoader is set by the sheet package to load external files.
// If nil, the default loader (loadExternalFile) is used.
var ExternalLoader func(filename string) (map[string]*ExternalCell, error)

// EvalExternal evaluates an expression against an external file.
//
// This is the entry point used by the sheet package's @evaler function.
func EvalExternal(filename, expr string) (*big.Rat, error) {
	loader := ExternalLoader
	if loader == nil {
		loader = loadExternalFile
	}

	cells, err := loader(filename)
	if err != nil {
		return nil, err
	}

	e := &externalEvaluator{
		cells:    cells,
		cache:    make(map[string]*big.Rat),
		visiting: make(map[string]bool),
	}
	return e.evalFormula(expr)
}

// ===========================================================================
// External evaluator
// ===========================================================================

type externalEvaluator struct {
	cells    map[string]*ExternalCell
	cache    map[string]*big.Rat
	visiting map[string]bool
}

func (e *externalEvaluator) evalCell(addr string) (*big.Rat, error) {
	if v, ok := e.cache[addr]; ok {
		return v, nil
	}
	if e.visiting[addr] {
		return nil, fmt.Errorf("circular reference at %s", addr)
	}
	c, ok := e.cells[addr]
	if !ok {
		r := new(big.Rat)
		e.cache[addr] = r
		return r, nil
	}
	e.visiting[addr] = true
	defer delete(e.visiting, addr)

	switch c.Kind {
	case 0: // number
		r := new(big.Rat)
		if _, err := fmt.Sscan(c.Value, r); err != nil {
			return nil, err
		}
		e.cache[addr] = r
		return r, nil
	case 2: // string
		r := new(big.Rat)
		e.cache[addr] = r
		return r, nil
	case 1: // formula
		r, err := e.evalFormula(c.Value)
		if err != nil {
			return nil, err
		}
		e.cache[addr] = r
		return r, nil
	}
	return nil, fmt.Errorf("unknown cell kind")
}

func (e *externalEvaluator) evalFormula(expr string) (*big.Rat, error) {
	tokens := extTokenize(expr)
	postfix := extToPostfix(tokens)
	return e.evalPostfix(postfix)
}

type externalValue struct {
	single *big.Rat
	list   []*big.Rat
}

func newExtSingle(r *big.Rat) externalValue { return externalValue{single: r} }
func newExtList(rs []*big.Rat) externalValue { return externalValue{list: rs} }

func (v externalValue) asList() []*big.Rat {
	if v.list != nil {
		return v.list
	}
	return []*big.Rat{v.single}
}

func (v externalValue) asSingle() *big.Rat {
	if v.single != nil {
		return v.single
	}
	sum := new(big.Rat)
	for _, r := range v.list {
		sum.Add(sum, r)
	}
	return sum
}

func (e *externalEvaluator) evalPostfix(postfix []string) (*big.Rat, error) {
	var stack []externalValue
	for _, tok := range postfix {
		if extIsFunction(tok) {
			parts := strings.SplitN(tok, "/", 2)
			fn := parts[0]
			n := 1
			if len(parts) == 2 {
				n, _ = strconv.Atoi(parts[1])
			}
			if len(stack) < n {
				return nil, fmt.Errorf("function %s needs %d arguments, got %d", fn, n, len(stack))
			}
			args := stack[len(stack)-n:]
			stack = stack[:len(stack)-n]

			var nums []*big.Rat
			for _, a := range args {
				nums = append(nums, a.asList()...)
			}

			val, err := extApplyFunction(fn, nums)
			if err != nil {
				return nil, err
			}
			stack = append(stack, newExtSingle(val))
			continue
		}

		if extIsOperator(tok) {
			if len(stack) < 2 {
				return nil, fmt.Errorf("malformed expression")
			}
			op2 := stack[len(stack)-1].asSingle()
			op1 := stack[len(stack)-2].asSingle()
			stack = stack[:len(stack)-2]
			r := new(big.Rat)
			switch tok {
			case "+":
				r.Add(op1, op2)
			case "-":
				r.Sub(op1, op2)
			case "*":
				r.Mul(op1, op2)
			case "/":
				if op2.Sign() == 0 {
					return nil, fmt.Errorf("division by zero")
				}
				r.Quo(op1, op2)
			case "**":
				f1, _ := op1.Float64()
				f2, _ := op2.Float64()
				r.SetFloat64(math.Pow(f1, f2))
			}
			stack = append(stack, newExtSingle(r))
			continue
		}

		if strings.Contains(tok, ":") {
			val, err := e.evalRangeList(tok)
			if err != nil {
				return nil, err
			}
			stack = append(stack, newExtList(val))
			continue
		}

		if extAddrRx.MatchString(tok) {
			val, err := e.evalCell(tok)
			if err != nil {
				return nil, err
			}
			stack = append(stack, newExtSingle(val))
			continue
		}

		r := new(big.Rat)
		if _, err := fmt.Sscan(tok, r); err != nil {
			return nil, fmt.Errorf("unknown token %q", tok)
		}
		stack = append(stack, newExtSingle(r))
	}

	if len(stack) != 1 {
		return nil, fmt.Errorf("malformed expression")
	}
	return stack[0].asSingle(), nil
}

func (e *externalEvaluator) evalRangeList(rng string) ([]*big.Rat, error) {
	parts := strings.SplitN(rng, ":", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("invalid range %q", rng)
	}
	m1 := extAddrRx.FindStringSubmatch(parts[0])
	m2 := extAddrRx.FindStringSubmatch(parts[1])
	if m1 == nil || m2 == nil {
		return nil, fmt.Errorf("invalid range %q", rng)
	}
	r1, _ := strconv.Atoi(m1[2])
	c1 := extColumnIndex(m1[1])
	r2, _ := strconv.Atoi(m2[2])
	c2 := extColumnIndex(m2[1])

	if r1 > r2 {
		r1, r2 = r2, r1
	}
	if c1 > c2 {
		c1, c2 = c2, c1
	}

	var values []*big.Rat
	for row := r1; row <= r2; row++ {
		for col := c1; col <= c2; col++ {
			addr := extColumnName(col) + strconv.Itoa(row)
			v, err := e.evalCell(addr)
			if err != nil {
				return nil, err
			}
			values = append(values, v)
		}
	}
	return values, nil
}

// ===========================================================================
// Tokenizer, parser and function application (self-contained)
// ===========================================================================

var extAddrRx = regexp.MustCompile(`^([A-Z]+)(\d+)$`)
var extCmdRx = regexp.MustCompile(`^(let|leftstring|rightstring|label)\s+([A-Z]+\d+)\s*=\s*(.*)$`)

func extIsOperator(tok string) bool {
	return tok == "+" || tok == "-" || tok == "*" || tok == "/" || tok == "**"
}

func extIsFunction(tok string) bool {
	return strings.HasPrefix(tok, "@sum/") ||
		strings.HasPrefix(tok, "@avg/") ||
		strings.HasPrefix(tok, "@max/") ||
		strings.HasPrefix(tok, "@min/")
}

func extPrec(op string) int {
	switch op {
	case "+", "-":
		return 1
	case "*", "/":
		return 2
	case "**":
		return 3
	}
	return 0
}

func extTokenize(expr string) []string {
	var tokens []string
	i := 0
	for i < len(expr) {
		c := expr[i]

		if c == ' ' || c == '\t' || c == ',' {
			i++
			continue
		}

		if c == '@' {
			start := i
			i++
			for i < len(expr) && ((expr[i] >= 'a' && expr[i] <= 'z') || (expr[i] >= 'A' && expr[i] <= 'Z')) {
				i++
			}
			fn := strings.ToLower(expr[start:i])

			j := i
			for j < len(expr) && (expr[j] == ' ' || expr[j] == '\t') {
				j++
			}

			if j < len(expr) && expr[j] == '(' {
				depth := 1
				k := j + 1
				argStart := k
				argCount := 1
				for k < len(expr) && depth > 0 {
					switch expr[k] {
					case '(':
						depth++
					case ')':
						depth--
					case ',':
						if depth == 1 {
							argCount++
						}
					}
					k++
				}
				argsExpr := expr[argStart : k-1]
				argTokens := extTokenize(argsExpr)
				tokens = append(tokens, argTokens...)
				tokens = append(tokens, fmt.Sprintf("%s/%d", fn, argCount))
				i = k
			} else {
				tokens = append(tokens, fn)
			}
			continue
		}

		if c >= 'A' && c <= 'Z' {
			start := i
			for i < len(expr) && expr[i] >= 'A' && expr[i] <= 'Z' {
				i++
			}
			for i < len(expr) && expr[i] >= '0' && expr[i] <= '9' {
				i++
			}
			if i < len(expr) && expr[i] == ':' {
				i++
				for i < len(expr) && expr[i] >= 'A' && expr[i] <= 'Z' {
					i++
				}
				for i < len(expr) && expr[i] >= '0' && expr[i] <= '9' {
					i++
				}
			}
			tokens = append(tokens, expr[start:i])
			continue
		}

		if c >= '0' && c <= '9' {
			start := i
			for i < len(expr) && ((expr[i] >= '0' && expr[i] <= '9') || expr[i] == '.') {
				i++
			}
			tokens = append(tokens, expr[start:i])
			continue
		}

		if c == '*' && i+1 < len(expr) && expr[i+1] == '*' {
			tokens = append(tokens, "**")
			i += 2
			continue
		}

		if strings.ContainsRune("-+*/()", rune(c)) {
			tokens = append(tokens, string(c))
			i++
			continue
		}

		i++
	}
	return tokens
}

func extToPostfix(tokens []string) []string {
	var out []string
	var ops []string

	for _, tok := range tokens {
		if extIsFunction(tok) {
			out = append(out, tok)
			continue
		}
		if extIsOperator(tok) {
			for len(ops) > 0 && ops[len(ops)-1] != "(" && extPrec(ops[len(ops)-1]) >= extPrec(tok) {
				out = append(out, ops[len(ops)-1])
				ops = ops[:len(ops)-1]
			}
			ops = append(ops, tok)
			continue
		}
		if tok == "(" {
			ops = append(ops, tok)
			continue
		}
		if tok == ")" {
			for len(ops) > 0 && ops[len(ops)-1] != "(" {
				out = append(out, ops[len(ops)-1])
				ops = ops[:len(ops)-1]
			}
			if len(ops) > 0 {
				ops = ops[:len(ops)-1]
			}
			continue
		}
		out = append(out, tok)
	}

	for len(ops) > 0 {
		out = append(out, ops[len(ops)-1])
		ops = ops[:len(ops)-1]
	}
	return out
}

func extApplyFunction(fn string, args []*big.Rat) (*big.Rat, error) {
	if len(args) == 0 {
		return new(big.Rat), nil
	}
	switch fn {
	case "@sum":
		sum := new(big.Rat)
		for _, v := range args {
			sum.Add(sum, v)
		}
		return sum, nil
	case "@avg":
		sum := new(big.Rat)
		for _, v := range args {
			sum.Add(sum, v)
		}
		return sum.Quo(sum, big.NewRat(int64(len(args)), 1)), nil
	case "@max":
		max := args[0]
		for _, v := range args[1:] {
			if v.Cmp(max) > 0 {
				max = v
			}
		}
		return max, nil
	case "@min":
		min := args[0]
		for _, v := range args[1:] {
			if v.Cmp(min) < 0 {
				min = v
			}
		}
		return min, nil
	}
	return nil, fmt.Errorf("unknown function %s", fn)
}

// ===========================================================================
// File loaders (self-contained)
// ===========================================================================

// loadExternalFile loads an sc or csv file into a map of ExternalCell.
func loadExternalFile(filename string) (map[string]*ExternalCell, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".csv", ".tsv":
		return loadExternalCSV(filename)
	case ".sc", ".txt", "":
		return loadExternalSC(filename)
	default:
		cells, err := loadExternalSC(filename)
		if err != nil {
			return loadExternalCSV(filename)
		}
		return cells, nil
	}
}

func loadExternalSC(filename string) (map[string]*ExternalCell, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	cells := make(map[string]*ExternalCell)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		m := extCmdRx.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cmd, addr, val := m[1], m[2], m[3]

		if cmd == "let" {
			if extIsNumeric(val) {
				cells[addr] = &ExternalCell{0, addr, val}
			} else {
				cells[addr] = &ExternalCell{1, addr, val}
			}
			continue
		}
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
			val = extUnescapeString(val)
		}
		cells[addr] = &ExternalCell{2, addr, val}
	}
	return cells, scanner.Err()
}

func loadExternalCSV(filename string) (map[string]*ExternalCell, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.LazyQuotes = true
	reader.FieldsPerRecord = -1
	reader.TrimLeadingSpace = true

	cells := make(map[string]*ExternalCell)
	row := 0
	for {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		for col, field := range record {
			if field == "" {
				continue
			}
			addr := extColumnName(col) + strconv.Itoa(row)
			if extIsNumeric(field) {
				cells[addr] = &ExternalCell{0, addr, field}
			} else if strings.HasPrefix(field, "=") {
				cells[addr] = &ExternalCell{1, addr, strings.TrimPrefix(field, "=")}
			} else {
				cells[addr] = &ExternalCell{2, addr, field}
			}
		}
		row++
	}
	return cells, nil
}

// ===========================================================================
// Helpers
// ===========================================================================

func extIsNumeric(s string) bool {
	if s == "" {
		return false
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	if _, err := strconv.ParseFloat(s, 64); err == nil {
		return true
	}
	return false
}

func extUnescapeString(s string) string {
	s = strings.ReplaceAll(s, `\"`, `"`)
	s = strings.ReplaceAll(s, `\\`, `\`)
	return s
}

func extColumnIndex(name string) int {
	col := 0
	for _, ch := range name {
		col = col*26 + int(ch-'A'+1)
	}
	return col - 1
}

func extColumnName(col int) string {
	if col < 0 {
		col = 0
	}
	name := ""
	for {
		name = string(rune('A'+col%26)) + name
		col = col/26 - 1
		if col < 0 {
			break
		}
	}
	return name
}
