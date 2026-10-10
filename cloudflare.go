package dgodriver

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/aijiesd520/dgodriver/cdp"
)

type CloudflareChallenge struct{ Host, ShadowRoot, IFrame *Element }

func (t *Tab) FindCloudflareChallenge(ctx context.Context) (*CloudflareChallenge, error) {
	root, err := t.document(ctx)
	if err != nil {
		return nil, err
	}
	var found *CloudflareChallenge
	walkNodes(root, func(host cdp.DOMNode) {
		if found != nil {
			return
		}
		for _, shadow := range host.ShadowRoots {
			walkNodes(shadow, func(node cdp.DOMNode) {
				if found != nil || node.NodeName != "IFRAME" {
					return
				}
				e := &Element{tab: t, node: node}
				if strings.Contains(e.Get("src"), "challenges.cloudflare.com") {
					found = &CloudflareChallenge{Host: &Element{tab: t, node: host}, ShadowRoot: &Element{tab: t, node: shadow}, IFrame: e}
				}
			})
		}
	})
	if found == nil {
		return nil, ErrNotFound
	}
	return found, nil
}
func (t *Tab) WaitForCloudflareChallenge(ctx context.Context) (*CloudflareChallenge, error) {
	var result *CloudflareChallenge
	err := poll(ctx, func() (bool, error) {
		var err error
		result, err = t.FindCloudflareChallenge(ctx)
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		p, err := result.IFrame.GetPosition(ctx)
		return p.Width > 0 && p.Height > 0, err
	})
	return result, err
}
func (t *Tab) IsCloudflareChallengePresent(ctx context.Context) (bool, error) {
	_, err := t.FindCloudflareChallenge(ctx)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

type CloudflareOptions struct {
	Timeout, ClickDelay time.Duration
	ChallengeSelector   string
	FlashCorners        bool
}

// VerifyCF ports zendriver's interactive checkbox helper. Success is observed
// through the response input receiving a value or disappearing after a click.
// Challenge provider behavior is external and can change independently.
func (t *Tab) VerifyCF(ctx context.Context, options ...CloudflareOptions) error {
	opt := CloudflareOptions{Timeout: 15 * time.Second, ClickDelay: 5 * time.Second}
	if len(options) > 0 {
		opt = options[0]
		if opt.Timeout <= 0 {
			opt.Timeout = 15 * time.Second
		}
		if opt.ClickDelay <= 0 {
			opt.ClickDelay = 5 * time.Second
		}
	}
	limited, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()
	challenge, err := t.WaitForCloudflareChallenge(limited)
	if err != nil {
		return err
	}
	if err = challenge.IFrame.ScrollIntoView(limited); err != nil {
		return err
	}
	p, err := challenge.IFrame.GetPosition(limited)
	if err != nil {
		return err
	}
	if opt.FlashCorners {
		for _, point := range [][2]float64{{p.Left, p.Top}, {p.Right, p.Top}, {p.Right, p.Bottom}, {p.Left, p.Bottom}} {
			if err = t.FlashPoint(limited, point[0], point[1], 10); err != nil {
				return err
			}
		}
	}
	selector := opt.ChallengeSelector
	if selector == "" {
		selector = "input[name=cf-turnstile-response],input[name=cf_challenge_response]"
	}
	if _, err = t.QuerySelector(limited, selector); err != nil {
		return err
	}
	clicked := false
	for {
		var state struct {
			Present bool
			Value   string
		}
		if err = t.Evaluate(limited, "(()=>{const el=document.querySelector("+jsString(selector)+");return {Present:!!el,Value:el?el.value:''}})()", &state); err != nil {
			return err
		}
		if state.Value != "" || (!state.Present && clicked) {
			return nil
		}
		if err = t.MouseClick(limited, p.X+p.Width*0.15, p.Y+p.Height/2); err != nil {
			return err
		}
		clicked = true
		if err = Sleep(limited, opt.ClickDelay); err != nil {
			return err
		}
	}
}
