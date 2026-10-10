package dgodriver

import (
	"context"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

func (b *Browser) Sleep(ctx context.Context, d time.Duration) error { return Sleep(ctx, d) }
func (b *Browser) TestConnection(ctx context.Context) error {
	_, err := cdp.Browser.GetVersion().Execute(ctx, b)
	return err
}
func WalkNodes(root cdp.DOMNode, fn func(cdp.DOMNode)) { walkNodes(root, fn) }
func FilterNodes(root cdp.DOMNode, predicate func(cdp.DOMNode) bool) []cdp.DOMNode {
	var result []cdp.DOMNode
	walkNodes(root, func(n cdp.DOMNode) {
		if predicate(n) {
			result = append(result, n)
		}
	})
	return result
}
func FlattenFrameTree(tree cdp.PageFrameTree) []cdp.PageFrameTree {
	result := []cdp.PageFrameTree{tree}
	for _, child := range tree.ChildFrames {
		result = append(result, FlattenFrameTree(child)...)
	}
	return result
}
func Circle(x, y, radius float64, num int, clockwise bool) [][2]float64 {
	if num <= 0 {
		return nil
	}
	points := make([][2]float64, num)
	direction := 1.0
	if clockwise {
		direction = -1
	}
	for i := range points {
		angle := direction * 2 * math.Pi * float64(i) / float64(num)
		points[i] = [2]float64{x + radius*math.Cos(angle), y + radius*math.Sin(angle)}
	}
	return points
}
func CompareTargetInfo(a, b cdp.TargetTargetInfo) map[string][2]any {
	result := map[string][2]any{}
	av, bv := reflect.ValueOf(a), reflect.ValueOf(b)
	typ := av.Type()
	for i := 0; i < av.NumField(); i++ {
		left, right := av.Field(i).Interface(), bv.Field(i).Interface()
		if !reflect.DeepEqual(left, right) {
			result[typ.Field(i).Name] = [2]any{left, right}
		}
	}
	return result
}
func (t *Tab) FindElementByText(ctx context.Context, text string, bestMatch ...bool) (*Element, error) {
	items, err := t.FindElementsByText(ctx, text)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	if len(bestMatch) == 0 || !bestMatch[0] {
		return items[0], nil
	}
	return bestTextMatch(ctx, items, text)
}
func bestTextMatch(ctx context.Context, items []*Element, text string) (*Element, error) {
	best := items[0]
	score := int(^uint(0) >> 1)
	for _, e := range items {
		s, err := e.TextAll(ctx)
		if err != nil {
			return nil, err
		}
		n := int(math.Abs(float64(len([]rune(strings.TrimSpace(s))) - len([]rune(text)))))
		if n < score {
			score = n
			best = e
		}
	}
	return best, nil
}
