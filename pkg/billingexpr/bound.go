package billingexpr

import (
	"fmt"
	"math"

	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
	"github.com/tidwall/gjson"
)

// NumericRange is an inclusive, finite interval. Independent intervals are
// deliberately conservative when token categories overlap. They are not samples.
type NumericRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// CostUpperBound proves a bound for the supported arithmetic pricing grammar.
// Every conditional branch participates, including branches skipped by the
// quoted request. Unbounded variables, division through zero and unsupported
// arithmetic fail closed; ordinary expression execution remains unchanged.
func CostUpperBound(expression string, variables, usage map[string]NumericRange, request RequestInput) (float64, error) {
	if _, err := CompileFromCache(expression); err != nil {
		return 0, err
	}
	_, body := ParseExprVersion(expression)
	tree, err := parser.Parse(body)
	if err != nil {
		return 0, err
	}
	b := costBounder{variables: variables, usage: usage, request: request}
	r, err := b.bound(tree.Node)
	if err != nil {
		return 0, err
	}
	if r.Min < 0 || math.IsNaN(r.Max) || math.IsInf(r.Max, 0) {
		return 0, fmt.Errorf("pricing expression has no proven nonnegative finite bound")
	}
	return r.Max, nil
}

type costBounder struct {
	variables, usage map[string]NumericRange
	request          RequestInput
}

func (b costBounder) bound(node ast.Node) (NumericRange, error) {
	var r NumericRange
	switch n := node.(type) {
	case *ast.IntegerNode:
		r = NumericRange{float64(n.Value), float64(n.Value)}
	case *ast.FloatNode:
		r = NumericRange{n.Value, n.Value}
	case *ast.IdentifierNode:
		var ok bool
		r, ok = b.variables[n.Value]
		if !ok {
			return r, fmt.Errorf("missing bound for %s", n.Value)
		}
	case *ast.UnaryNode:
		x, err := b.bound(n.Node)
		if err != nil {
			return r, err
		}
		switch n.Operator {
		case "+":
			r = x
		case "-":
			r = NumericRange{-x.Max, -x.Min}
		default:
			return r, fmt.Errorf("unsupported unary bound")
		}
	case *ast.ConditionalNode:
		x, err := b.bound(n.Exp1)
		if err != nil {
			return r, err
		}
		y, err := b.bound(n.Exp2)
		if err != nil {
			return r, err
		}
		r = NumericRange{min(x.Min, y.Min), max(x.Max, y.Max)}
	case *ast.BinaryNode:
		x, err := b.bound(n.Left)
		if err != nil {
			return r, err
		}
		y, err := b.bound(n.Right)
		if err != nil {
			return r, err
		}
		switch n.Operator {
		case "+":
			r = NumericRange{x.Min + y.Min, x.Max + y.Max}
		case "-":
			r = NumericRange{x.Min - y.Max, x.Max - y.Min}
		case "*", "/":
			if n.Operator == "/" {
				if y.Min <= 0 && y.Max >= 0 {
					return r, fmt.Errorf("division includes zero")
				}
				y = NumericRange{1 / y.Max, 1 / y.Min}
			}
			v := []float64{x.Min * y.Min, x.Min * y.Max, x.Max * y.Min, x.Max * y.Max}
			r = NumericRange{v[0], v[0]}
			for _, f := range v[1:] {
				r.Min = min(r.Min, f)
				r.Max = max(r.Max, f)
			}
		default:
			return r, fmt.Errorf("unsupported price arithmetic %s", n.Operator)
		}
	case *ast.CallNode:
		id, ok := n.Callee.(*ast.IdentifierNode)
		if !ok {
			return r, fmt.Errorf("unsupported pricing call")
		}
		name := id.Value
		if name == "u" || name == "param" {
			if len(n.Arguments) != 1 {
				return r, fmt.Errorf("invalid price probe")
			}
			key, ok := n.Arguments[0].(*ast.StringNode)
			if !ok {
				return r, fmt.Errorf("dynamic price probe")
			}
			if name == "u" {
				var found bool
				r, found = b.usage[key.Value]
				if !found {
					return r, fmt.Errorf("missing usage bound %s", key.Value)
				}
			} else {
				v := gjson.GetBytes(b.request.Body, key.Value)
				if v.Type != gjson.Number {
					return r, fmt.Errorf("non-numeric price parameter")
				}
				r = NumericRange{v.Float(), v.Float()}
			}
			break
		}
		args := n.Arguments
		if name == "tier" {
			if len(args) != 2 {
				return r, fmt.Errorf("invalid tier")
			}
			return b.bound(args[1])
		}
		if len(args) < 1 || len(args) > 2 {
			return r, fmt.Errorf("unsupported price function %s", name)
		}
		x, err := b.bound(args[0])
		if err != nil {
			return r, err
		}
		switch name {
		case "fixed":
			r = NumericRange{x.Min * 1e6, x.Max * 1e6}
		case "abs":
			r = NumericRange{min(math.Abs(x.Min), math.Abs(x.Max)), max(math.Abs(x.Min), math.Abs(x.Max))}
			if x.Min <= 0 && x.Max >= 0 {
				r.Min = 0
			}
		case "floor":
			r = NumericRange{math.Floor(x.Min), math.Floor(x.Max)}
		case "ceil":
			r = NumericRange{math.Ceil(x.Min), math.Ceil(x.Max)}
		case "min", "max":
			if len(args) != 2 {
				return r, fmt.Errorf("missing price argument")
			}
			y, err := b.bound(args[1])
			if err != nil {
				return r, err
			}
			if name == "min" {
				r = NumericRange{min(x.Min, y.Min), min(x.Max, y.Max)}
			} else {
				r = NumericRange{max(x.Min, y.Min), max(x.Max, y.Max)}
			}
		default:
			return r, fmt.Errorf("unsupported price function %s", name)
		}
	case *ast.BuiltinNode:
		return b.bound(&ast.CallNode{Callee: &ast.IdentifierNode{Value: n.Name}, Arguments: n.Arguments})
	default:
		return r, fmt.Errorf("unsupported price expression %T", node)
	}
	if math.IsNaN(r.Min) || math.IsNaN(r.Max) || math.IsInf(r.Min, 0) || math.IsInf(r.Max, 0) || r.Min > r.Max {
		return r, fmt.Errorf("nonfinite or invalid price interval")
	}
	// Float arithmetic rounds each intermediate result; outward rounding keeps
	// the proof conservative across chained operations. Exact zero stays free.
	if r.Min != 0 {
		r.Min = math.Nextafter(r.Min, math.Inf(-1))
	}
	if r.Max != 0 {
		r.Max = math.Nextafter(r.Max, math.Inf(1))
	}
	return r, nil
}
