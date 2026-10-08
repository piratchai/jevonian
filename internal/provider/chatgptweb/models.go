package chatgptweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/chromedp/chromedp"
)

// fetchModelsJS runs in the authenticated browser origin (chatgpt.com).
// Tokens NEVER leave the browser or get logged.
const fetchModelsJS = `(async () => {
  try {
    const sessionRes = await fetch('/api/auth/session', { credentials: 'include' });
    if (!sessionRes.ok) {
      return { ok: false, errorKind: 'auth', errorMsg: 'session check returned HTTP ' + sessionRes.status };
    }
    const sessionData = await sessionRes.json();
    if (!sessionData || !sessionData.accessToken) {
      return { ok: false, errorKind: 'auth', errorMsg: 'not logged in or accessToken missing' };
    }
    const token = sessionData.accessToken;

    const modelsRes = await fetch('/backend-api/models?iim=false&is_gizmo=false', {
      headers: { 'Authorization': 'Bearer ' + token }
    });
    if (!modelsRes.ok) {
      return { ok: false, errorKind: 'model', errorMsg: 'models fetch returned HTTP ' + modelsRes.status };
    }
    const raw = await modelsRes.json();
    const list = raw.models || raw || [];

    const models = [];
    for (const item of list) {
      if (item.enabled === false) continue;
      if (item.is_hidden === true) continue;
      if (item.disabled === true) continue;
      const id = item.slug || item.id || '';
      if (!id) continue;
      const title = item.title || item.display_name || item.name || id;
      models.push({ id: id, title: title });
    }

    return { ok: true, models: models };
  } catch (e) {
    return { ok: false, errorKind: 'browser', errorMsg: e.toString() };
  }
})()`

type fetchModelsResult struct {
	OK        bool    `json:"ok"`
	ErrorKind Kind    `json:"errorKind,omitempty"`
	ErrorMsg  string  `json:"errorMsg,omitempty"`
	Models    []Model `json:"models,omitempty"`
}

// FetchAccountModels retrieves the list of active models directly from the browser context.
func FetchAccountModels(ctx context.Context) ([]Model, *Error) {
	rawJSON, err := chromedp.Run(ctx,
		chromedp.Evaluate[[]byte](fetchModelsJS, chromedp.EvalAwaitPromise),
	)
	if err != nil {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to evaluate models in browser: %v", err),
		}
	}

	var res fetchModelsResult
	if err := json.Unmarshal([]byte(rawJSON), &res); err != nil {
		return nil, &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to decode models response from browser: %v", err),
		}
	}

	if !res.OK {
		status := http.StatusBadGateway
		if res.ErrorKind == KindAuth {
			status = http.StatusUnauthorized
		} else if res.ErrorKind == KindRateLimit {
			status = http.StatusTooManyRequests
		}
		return nil, &Error{
			Status:  status,
			Kind:    res.ErrorKind,
			Message: res.ErrorMsg,
		}
	}

	return res.Models, nil
}

// selectModelJS matches exact slug/data attribute or EXACT catalog title in live UI picker.
const selectModelJS = `(async (targetSlug, targetTitle) => {
  const pickerSelectors = [
    'button[data-codex-intelligence-trigger="true"][aria-haspopup="menu"]',
    '#model-selector-btn',
    'button[data-testid*="model-switcher"]',
    'button[data-testid*="model-selector"]',
    'button[aria-label*="Model" i]'
  ];

  const findPicker = () => {
    for (const sel of pickerSelectors) {
      const el = [...document.querySelectorAll(sel)].find(node => node.offsetParent !== null);
      if (el) return el;
    }
    // Current ChatGPT composer uses a combined model/effort menu trigger.
    return [...document.querySelectorAll('button[aria-haspopup="menu"]')].find(el =>
      el.offsetParent !== null && el.hasAttribute('data-codex-intelligence-trigger')
    ) || null;
  };
  let btn = findPicker();
  for (let i = 0; !btn && i < 20; i++) {
    await new Promise(r => setTimeout(r, 250));
    btn = findPicker();
  }
  if (!btn) {
    return { ok: false, reason: 'model picker button not found in UI after waiting at ' + location.pathname };
  }

  const slugLower = (targetSlug || '').trim().toLowerCase();
  const titleLower = (targetTitle || '').trim().toLowerCase();
  const session = await fetch('/api/auth/session', { credentials: 'include' }).then(r => r.json());
  const catalogResponse = await fetch('/backend-api/models?iim=false&is_gizmo=false', {
    headers: { Authorization: 'Bearer ' + session.accessToken }
  });
  if (!catalogResponse.ok) return { ok: false, reason: 'could not verify live model catalog' };
  const catalogData = await catalogResponse.json();
  const argumentsModels = (catalogData.models || catalogData || []).map(model => ({
    id: String(model.slug || model.id || ''),
    title: String(model.title || model.display_name || model.name || model.slug || model.id || '')
  }));

  btn.click();
  await new Promise(r => setTimeout(r, 500));

  // The current picker nests the model list behind a “Select model” view toggle.
  let viewToggle = document.querySelector('[data-model-picker-view-toggle]');
  if (!viewToggle) {
    viewToggle = [...document.querySelectorAll('[role="menuitem"]')].find(el =>
      (el.getAttribute('aria-label') || '').trim().toLowerCase() === 'select model'
    ) || null;
  }
  if (viewToggle) {
    viewToggle.click();
    await new Promise(r => setTimeout(r, 400));
  }
  const versions = catalogData.versions || [];
  const versionForSlug = versions.find(version => (version.slugs || []).includes(slugLower));
  const pickerTitle = String(versionForSlug && (versionForSlug.display_text_for_intelligence || versionForSlug.display_text) || titleLower).trim().toLowerCase();
  const presets = versionForSlug && Array.isArray(versionForSlug.intelligence_presets) ? versionForSlug.intelligence_presets : [];
  let selectedPreset = presets.find(preset => preset.model_slug === slugLower);
  if (selectedPreset && selectedPreset.title === 'High' && presets.some(preset => preset.model_slug === slugLower && preset.title === 'Medium')) {
    selectedPreset = presets.find(preset => preset.model_slug === slugLower && preset.title === 'Medium');
  }
  const variantIndex = selectedPreset ? presets.indexOf(selectedPreset) : -1;


  const itemSelectors = [
    '[role="menuitemradio"]',
    '[role="option"]',
    'button[data-slug]',
    'button[data-testid*="model"]',
    'div[class*="model"] button',
    'li[class*="model"]'
  ];
  const items = document.querySelectorAll(itemSelectors.join(', '));
  let matched = null;
  const exactCatalogEntry = argumentsModels.find(model =>
    model.id.toLowerCase() === slugLower && model.title.trim().toLowerCase() === titleLower
  );
  if (!exactCatalogEntry) {
    document.body.click();
    return { ok: false, reason: 'requested model ID is not present in the live catalog' };
  }
  if (!versionForSlug || !versionForSlug.enabled || !versionForSlug.slugs.includes(slugLower)) {
    document.body.click();
    return { ok: false, reason: 'model has no enabled version entry in the live picker catalog' };
  }
  for (const item of items) {
    const slug = (item.getAttribute('data-slug') || item.getAttribute('data-model') || '').trim().toLowerCase();
    const testid = (item.getAttribute('data-testid') || '').trim().toLowerCase();
    const text = (item.textContent || '').trim().toLowerCase();
    const exactSelected = item.getAttribute('data-model-selected') === 'true';

    // Some newer ChatGPT pickers list model families only. We select the
    // exact family here, then set the exact lane/effort in the composer menu.
    if (slug && slug === slugLower) {
      matched = item;
      break;
    }
    if (testid && (testid === 'model-' + slugLower || testid === slugLower)) {
      matched = item;
      break;
    }
    if (text === pickerTitle) {
      matched = item;
      break;
    }
  }

  if (!matched) {
    document.body.click(); // close dropdown
    return { ok: false, reason: 'requested model not found in live UI picker options' };
  }

  matched.click();
  await new Promise(r => setTimeout(r, 350));

  if (variantIndex >= 0) {
    const effortButton = [...document.querySelectorAll('button[data-codex-intelligence-trigger="true"]')]
      .find(el => el.getAttribute('data-composer-navigation-target') === 'reasoning');
    if (!effortButton) return { ok: false, reason: 'model family selected, but reasoning-effort control is missing' };
    effortButton.click();
    await new Promise(r => setTimeout(r, 250));
    const slider = document.querySelector('[data-model-picker-power-slider] [role="slider"]');
    if (!slider) return { ok: false, reason: 'model family selected, but effort slider is missing' };
    const max = Number(slider.getAttribute('aria-valuemax'));
    if (!Number.isFinite(max) || max < 1) return { ok: false, reason: 'effort slider metadata is invalid' };
    const desiredValue = Math.max(0, Math.min(max, variantIndex));
    const currentValue = Number(slider.getAttribute('aria-valuenow'));
    const delta = desiredValue - currentValue;
    const key = delta < 0 ? 'ArrowLeft' : 'ArrowRight';
    for (let i = 0; i < Math.abs(delta); i++) {
      slider.focus();
      slider.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
      await new Promise(r => setTimeout(r, 40));
    }
    if (Number(slider.getAttribute('aria-valuenow')) !== desiredValue) return { ok: false, reason: 'effort control did not confirm requested lane' };
  }

  const confirmedBtn = document.querySelector(pickerSelectors.join(', ')) ||
    [...document.querySelectorAll('button[data-codex-intelligence-trigger="true"][aria-haspopup="menu"]')]
      .find(el => el.offsetParent !== null);
  if (!confirmedBtn) {
    return { ok: false, reason: 'model selection confirmation button not found' };
  }
  const confirmedText = (confirmedBtn.textContent || '').trim().toLowerCase();
  const confirmedSlug = (confirmedBtn.getAttribute('data-slug') || confirmedBtn.getAttribute('data-model') || '').trim().toLowerCase();
  const confirmedTestId = (confirmedBtn.getAttribute('data-testid') || '').trim().toLowerCase();
  const confirmedBySlug = confirmedSlug === slugLower || confirmedTestId === 'model-' + slugLower || confirmedTestId === slugLower;
  const selectedItem = document.querySelector('[role="menuitemradio"][data-model-selected="true"]');
  const confirmedByExactTitle = titleLower !== '' && confirmedText === titleLower;
  const confirmedBySelectedItem = selectedItem !== null && (selectedItem.textContent || '').trim().toLowerCase() === pickerTitle;
  if (!confirmedBySlug && !confirmedByExactTitle && !confirmedBySelectedItem) {
    return { ok: false, reason: 'model selection could not be confirmed in UI: button shows "' + confirmedText + '"' };
  }
  // Close the picker before typing so its overlay cannot intercept Send.
  document.body.click();
  await new Promise(r => setTimeout(r, 500));
  return { ok: true, selectedSlug: confirmedSlug || null, selectedTitle: confirmedText, selectedFamily: pickerTitle, selectedVariant: selectedPreset ? selectedPreset.title : '' };

})`

type selectModelResult struct {
	OK              bool   `json:"ok"`
	Reason          string `json:"reason,omitempty"`
	SelectedSlug    string `json:"selectedSlug,omitempty"`
	SelectedTitle   string `json:"selectedTitle,omitempty"`
	SelectedFamily  string `json:"selectedFamily,omitempty"`
	SelectedVariant string `json:"selectedVariant,omitempty"`
}

// SelectModelInUI strictly opens the UI model picker and confirms selection of target model.
func SelectModelInUI(ctx context.Context, targetSlug, targetTitle string) *Error {
	expr := fmt.Sprintf("(%s)(%q, %q)", selectModelJS, targetSlug, targetTitle)
	rawJSON, err := chromedp.Run(ctx,
		chromedp.Evaluate[[]byte](expr, chromedp.EvalAwaitPromise),
	)
	if err != nil {
		return &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to execute UI model selection script: %v", err),
		}
	}

	var res selectModelResult
	if err := json.Unmarshal([]byte(rawJSON), &res); err != nil {
		return &Error{
			Status:  http.StatusBadGateway,
			Kind:    KindBrowser,
			Message: fmt.Sprintf("failed to decode UI model selection result: %v", err),
		}
	}

	if !res.OK {
		return &Error{
			Status:  http.StatusBadRequest,
			Kind:    KindModel,
			Message: res.Reason,
		}
	}
	if res.SelectedSlug != "" && !strings.EqualFold(res.SelectedSlug, targetSlug) {
		return &Error{Status: http.StatusBadRequest, Kind: KindModel, Message: fmt.Sprintf("UI selected model %q, but requested %q", res.SelectedSlug, targetSlug)}
	}

	return nil
}

// FindModelInCatalog checks if a model ID or title exists in the provided catalog.
func FindModelInCatalog(catalog []Model, requested string) *Model {
	reqLower := strings.TrimSpace(strings.ToLower(requested))
	if reqLower == "" {
		return nil
	}
	for i := range catalog {
		if strings.ToLower(catalog[i].ID) == reqLower || strings.ToLower(catalog[i].Title) == reqLower {
			return &catalog[i]
		}
	}
	return nil
}

// canonicalModelSlug normalizes a model slug for lane comparison. ChatGPT
// canonicalizes some lane slugs before the outbound request. The instant lane
// is sent as the base family slug, so "gpt-5-6-instant" and "gpt-5-6" refer to
// the same lane.
func canonicalModelSlug(slug string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(slug)), "-instant")
}

// ModelMatchesRequested reports whether the observed outbound model satisfies
// the requested model. It accepts the exact slug and ChatGPT's canonical lane
// aliases so verification stays strict without rejecting a correct lane.
func ModelMatchesRequested(requested, observed string) bool {
	if observed == "" {
		return false
	}
	if strings.EqualFold(requested, observed) {
		return true
	}
	return canonicalModelSlug(requested) == canonicalModelSlug(observed)
}

// SelectModelByNavigation selects the target model by navigating to the
// model-scoped ChatGPT URL. This is reliable because the composer picker lists
// model families, and its optimistic state can silently retain a different
// lane. The URL query parameter resolves to the exact family and lane.
func SelectModelByNavigation(ctx context.Context, tabCtx context.Context, slug string) *Error {
	target := strings.TrimSpace(slug)
	if target == "" || strings.EqualFold(target, "auto") {
		return nil
	}
	navURL := "https://chatgpt.com/?model=" + url.QueryEscape(target)
	result, navErr := navigatePage(ctx, tabCtx, navURL)
	if navErr != nil {
		return navErr
	}
	if err := waitForNavigationReady(ctx, tabCtx, result.LoaderID); err != nil {
		return err
	}
	if err := ensureChatWorkspace(tabCtx); err != nil {
		return err
	}
	if err := waitForPageReady(ctx, tabCtx); err != nil {
		return err
	}
	return nil
}
