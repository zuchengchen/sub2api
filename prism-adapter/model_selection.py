"""Select account-visible Prism options before arming any model request.

The official page authors the request. Selecting a label is not permission to
substitute a model: both browser executors also verify outbound metadata.
"""
import re
from playwright.sync_api import TimeoutError as PlaywrightTimeoutError


MODELS = {
    'gpt-6.1-sol': '6.1 Sol',
}
EFFORTS = {
    'low': r'(?:Low|低)',
    'medium': r'(?:Medium|中|中等)',
    'high': r'(?:High|高)',
    'xhigh': r'(?:Extra[ -]?high|Very high|Xhigh|极高|最高)',
}
TRIGGER = re.compile(r'\d+(?:\.\d+)?\s+(?:Sol|Terra|Luna|Astra)(?:\s|$)')
MODEL_MENU = re.compile(r'^(?:Model|模型)(?:\s|$)', re.I)
EFFORT_MENU = re.compile(r'^(?:Effort|Reasoning effort|推理强度)(?:\s|$)', re.I)
OPTION_TIMEOUT = 10000

# Prism can finish SDK initialization while its React provider still holds
# isLoading=true. A same-user SDK update refreshes the provider from the real
# account evaluations. Never override gates, model lists or user attributes.
CATALOG_READY = """() => {
  const clients = Object.values(window.__STATSIG__?.instances || {}).filter(c =>
    typeof c.getContext === 'function' && typeof c.updateUserAsync === 'function');
  return clients.length === 1 && clients[0].loadingStatus === 'Ready';
}"""
REFRESH_CATALOG = """async () => {
  const clients = Object.values(window.__STATSIG__?.instances || {}).filter(c =>
    typeof c.getContext === 'function' && typeof c.updateUserAsync === 'function');
  if (clients.length !== 1 || clients[0].loadingStatus !== 'Ready') return false;
  const client = clients[0], user = client.getContext().user;
  if (!user || typeof user.userID !== 'string' || !user.userID) return false;
  await client.updateUserAsync(user, {timeoutMs:8000});
  await new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve)));
  return client.loadingStatus === 'Ready';
}"""


def patterns(model, effort):
    return (re.compile(r'^' + re.escape(MODELS[model]) + r'(?:\s|$)'),
            re.compile(r'\s' + EFFORTS[effort] + r'\s*$', re.I))


def selection_error(error, kind, value):
    return error(422, kind + '_unavailable',
                 'Requested Prism ' + kind + ' is not selectable in this account: ' + value + '; no model request was submitted')


def select_options(page, model, effort, error):
    try:
        page.wait_for_function(CATALOG_READY, timeout=30000)
        if page.evaluate(REFRESH_CATALOG) is not True:
            raise ValueError('catalog unavailable')
    except Exception:
        raise error(503, 'model_catalog_unavailable', 'Prism account model catalog is not ready; no model request was submitted') from None
    model_pattern, effort_pattern = patterns(model, effort)
    trigger = page.get_by_role('button', name=TRIGGER)
    trigger.wait_for(state='visible', timeout=60000)
    if not model_pattern.search(trigger.inner_text()):
        try:
            trigger.click(timeout=OPTION_TIMEOUT)
            page.get_by_role('menuitem', name=MODEL_MENU).hover(timeout=OPTION_TIMEOUT)
            page.get_by_role('menuitem', name=MODELS[model], exact=True).click(timeout=OPTION_TIMEOUT)
        except (PlaywrightTimeoutError, TimeoutError):
            raise selection_error(error, 'model', model) from None
    if not effort_pattern.search(trigger.inner_text()):
        try:
            trigger.click(timeout=OPTION_TIMEOUT)
            page.get_by_role('menuitem', name=EFFORT_MENU).hover(timeout=OPTION_TIMEOUT)
            page.get_by_role('menuitem', name=re.compile('^' + EFFORTS[effort] + '$', re.I)).click(timeout=OPTION_TIMEOUT)
        except (PlaywrightTimeoutError, TimeoutError):
            raise selection_error(error, 'reasoning', effort) from None
    label = trigger.inner_text()
    if not model_pattern.search(label) or not effort_pattern.search(label):
        raise error(502, 'selection_mismatch', 'Prism did not retain the requested model and effort; no model request was submitted')


async def select_options_async(page, model, effort, error):
    try:
        await page.wait_for_function(CATALOG_READY, timeout=30000)
        if await page.evaluate(REFRESH_CATALOG) is not True:
            raise ValueError('catalog unavailable')
    except Exception:
        raise error(503, 'model_catalog_unavailable', 'Prism account model catalog is not ready; no model request was submitted') from None
    model_pattern, effort_pattern = patterns(model, effort)
    trigger = page.get_by_role('button', name=TRIGGER)
    await trigger.wait_for(state='visible', timeout=60000)
    if not model_pattern.search(await trigger.inner_text()):
        try:
            await trigger.click(timeout=OPTION_TIMEOUT)
            await page.get_by_role('menuitem', name=MODEL_MENU).hover(timeout=OPTION_TIMEOUT)
            await page.get_by_role('menuitem', name=MODELS[model], exact=True).click(timeout=OPTION_TIMEOUT)
        except (PlaywrightTimeoutError, TimeoutError):
            raise selection_error(error, 'model', model) from None
    if not effort_pattern.search(await trigger.inner_text()):
        try:
            await trigger.click(timeout=OPTION_TIMEOUT)
            await page.get_by_role('menuitem', name=EFFORT_MENU).hover(timeout=OPTION_TIMEOUT)
            await page.get_by_role('menuitem', name=re.compile('^' + EFFORTS[effort] + '$', re.I)).click(timeout=OPTION_TIMEOUT)
        except (PlaywrightTimeoutError, TimeoutError):
            raise selection_error(error, 'reasoning', effort) from None
    label = await trigger.inner_text()
    if not model_pattern.search(label) or not effort_pattern.search(label):
        raise error(502, 'selection_mismatch', 'Prism did not retain the requested model and effort; no model request was submitted')
