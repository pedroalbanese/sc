package sheet

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/pedroalbanese/sc/internal/display"
	"github.com/pedroalbanese/sc/internal/evaler"
	"github.com/pedroalbanese/sc/internal/sheet/align"
)

type Cell struct {
	value      string
	stringType bool
	alignment  align.Align

	forwardRefs map[Address]struct{}

	cachedValue string
	cacheValid  bool
	calculating bool
}

func NewCell(value string, alignment align.Align, stringType bool) *Cell {
	return &Cell{
		value:       value,
		alignment:   alignment,
		stringType:  stringType,
		forwardRefs: make(map[Address]struct{}),
		cacheValid:  false,
	}
}

func (c *Cell) Copy(oldAddress, newAddress Address) *Cell {
	val := c.value
	if !c.stringType {
		rowDiff := newAddress.Row() - oldAddress.Row()
		colDiff := newAddress.Column() - oldAddress.Column()
		for forRef := range c.forwardRefs {
			refRow, refCol := forRef.RowCol()
			newRef := NewAddress(refRow+rowDiff, refCol+colDiff)
			val = strings.Replace(val, string(forRef), string(newRef), -1)
		}
	}
	return NewCell(val, c.alignment, c.stringType)
}

func (c *Cell) getDisplayValue(s *Sheet, address Address) string {
	// Cells that use @evaler are never cached, because the external
	// file may have changed since the last calculation.
	isExternal := strings.Contains(c.value, "@evaler(")

	if c.cacheValid && !isExternal {
		return c.cachedValue
	}
	if c.calculating {
		return "circle"
	}

	// Empty cell: return empty string, not "0.00".
	if c.value == "" {
		c.cachedValue = ""
		c.cacheValid = true
		return ""
	}

	c.calculating = true
	defer func() { c.calculating = false }()

	// Register the current sheet so RangeExpander can access it.
	prevSheet := currentSheet
	currentSheet = s
	defer func() { currentSheet = prevSheet }()

	// Handle @evaler(filename, expr) directly.
	if rat, ok := c.evalExternalCall(); ok {
		if rat == nil {
			if !isExternal {
				c.cachedValue = "INVALID"
				c.cacheValid = true
			}
			return "INVALID"
		}
		result := rat.FloatString(s.getPrecision(address))
		if !isExternal {
			c.cachedValue = result
			c.cacheValid = true
		}
		return result
	}

	postfix := evaler.GetPostfix(c.value)

	// Check if the postfix contains @ERR. If it does, skip propagation.
	hasErr := false
	for _, token := range postfix {
		if token == "@ERR" {
			hasErr = true
			break
		}
	}

	// Substitute cell addresses with their values. Ranges are left as
	// tokens; the evaler expands them via RangeExpander.
	for idx, token := range postfix {
		if evaler.IsCellAddr(token) {
			refCellVal := "0"
			if cell, err := s.GetCell(Address(token)); err == nil && cell != nil {
				v := cell.getDisplayValue(s, Address(token))
				if v == "circle" {
					if !isExternal {
						c.cachedValue = "circle"
						c.cacheValid = true
					}
					return "circle"
				}
				if !hasErr {
					if v == "ERROR" {
						if !isExternal {
							c.cachedValue = "INVALID"
							c.cacheValid = true
						}
						return "INVALID"
					}
					if v == "INVALID" {
						if !isExternal {
							c.cachedValue = "INVALID"
							c.cacheValid = true
						}
						return "INVALID"
					}
				}
				if v != "" && v != "ERROR" && v != "INVALID" {
					refCellVal = v
				}
			}
			postfix[idx] = refCellVal
		}
	}

	result := ""
	if rat, err := evaler.EvaluatePostfix(postfix); err == nil {
		result = rat.FloatString(s.getPrecision(address))
	} else if err == evaler.ErrRef {
		result = "ERROR"
	} else {
		result = "INVALID"
	}
	if !isExternal {
		c.cachedValue = result
		c.cacheValid = true
	}
	return result
}

// evalExternalCall checks if the cell's formula is an @evaler(...) call.
// If so, it evaluates the expression against the external file and
// returns the result. If the formula is not an @evaler call, it returns
// (nil, false).
//
// The syntax is: @evaler("filename", "expression")
// The quotes around the filename and expression are optional.
func (c *Cell) evalExternalCall() (*big.Rat, bool) {
	expr := strings.TrimSpace(c.value)
	if !strings.HasPrefix(expr, "@evaler(") {
		return nil, false
	}
	if !strings.HasSuffix(expr, ")") {
		return nil, true // malformed, but it's an @evaler call
	}

	// Extract the arguments inside the parentheses.
	inner := strings.TrimSuffix(strings.TrimPrefix(expr, "@evaler("), ")")

	// Split on the first top-level comma.
	parts := splitTopLevel(inner, ',')
	if len(parts) != 2 {
		return nil, true
	}

	filename := strings.TrimSpace(parts[0])
	filename = strings.Trim(filename, `"'`)
	subExpr := strings.TrimSpace(parts[1])
	subExpr = strings.Trim(subExpr, `"'`)

	rat, err := evaler.EvalExternal(filename, subExpr)
	if err != nil {
		return nil, true
	}
	return rat, true
}

// splitTopLevel splits a string on a separator, ignoring separators
// inside parentheses or quotes.
func splitTopLevel(s string, sep rune) []string {
	var out []string
	var cur strings.Builder
	depth := 0
	inSingle, inDouble := false, false
	for _, c := range s {
		switch c {
		case '(':
			if !inSingle && !inDouble {
				depth++
			}
			cur.WriteRune(c)
		case ')':
			if !inSingle && !inDouble {
				depth--
			}
			cur.WriteRune(c)
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
			cur.WriteRune(c)
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
			cur.WriteRune(c)
		case sep:
			if depth == 0 && !inSingle && !inDouble {
				out = append(out, cur.String())
				cur.Reset()
			} else {
				cur.WriteRune(c)
			}
		default:
			cur.WriteRune(c)
		}
	}
	out = append(out, cur.String())
	return out
}

// expandRangeList expands a range like A1:B3 into a list of cell values.
// Used by the evaler's RangeExpander.
func (s *Sheet) expandRangeList(rng string) []string {
	parts := strings.SplitN(rng, ":", 2)
	if len(parts) != 2 {
		return []string{"0"}
	}
	start := Address(parts[0])
	end := Address(parts[1])
	r1, c1 := start.RowCol()
	r2, c2 := end.RowCol()

	if r1 > r2 {
		r1, r2 = r2, r1
	}
	if c1 > c2 {
		c1, c2 = c2, c1
	}

	var vals []string
	for r := r1; r <= r2; r++ {
		for c := c1; c <= c2; c++ {
			addr := NewAddress(r, c)
			cell, err := s.GetCell(addr)
			if err != nil || cell == nil || cell.stringType {
				continue
			}
			v := cell.getDisplayValue(s, addr)
			if v == "circle" {
				return []string{"0"}
			}
			if v == "" {
				v = "0"
			}
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		return []string{"0"}
	}
	return vals
}

func (c *Cell) display(s *Sheet, address Address, row, colStart, colEnd int, selected bool) {
	dispVal := c.getDisplay(s, address)
	display.DisplayValue(dispVal, row, colStart, colEnd, c.alignment, selected)
}

func (c *Cell) getDisplay(s *Sheet, addr Address) string {
	if c.stringType {
		return c.value
	}
	return c.getDisplayValue(s, addr)
}

func (c *Cell) StatusBarVal() string {
	if c.stringType {
		modifier := ""
		switch c.alignment {
		case align.AlignLeft:
			modifier = "<"
		case align.AlignCenter:
			modifier = "|"
		default:
			modifier = ">"
		}
		return fmt.Sprintf("%s\"%s\"", modifier, c.value)
	}
	return c.value
}

// expandRange sums a range like A1:B3. Used only for formulas without
// functions; when a function is present, the evaler expands the range
// into individual values via RangeExpander.
func (s *Sheet) expandRange(rng string) string {
	parts := strings.SplitN(rng, ":", 2)
	if len(parts) != 2 {
		return "0"
	}
	start := Address(parts[0])
	end := Address(parts[1])
	r1, c1 := start.RowCol()
	r2, c2 := end.RowCol()

	sum := new(big.Rat)
	for r := r1; r <= r2; r++ {
		for c := c1; c <= c2; c++ {
			addr := NewAddress(r, c)
			cell, err := s.GetCell(addr)
			if err != nil || cell == nil || cell.stringType {
				continue
			}
			valStr := cell.getDisplayValue(s, addr)
			if valStr == "circle" {
				return "circle"
			}
			if valStr == "" {
				continue
			}
			v := new(big.Rat)
			if _, err := fmt.Sscan(valStr, v); err == nil {
				sum.Add(sum, v)
			}
		}
	}
	return sum.FloatString(10)
}
