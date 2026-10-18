import json
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from playwright.sync_api import expect, sync_playwright


class Handler(SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory=str(Path(__file__).parents[1] / 'client/index'), **kwargs)

    def log_message(self, *args):
        pass


def download(letter):
    return {
        'id': letter * 40, 'magnet': 'magnet:?xt=urn:btih:' + letter * 40,
        'title': letter.upper() + '.mkv', 'status': 'preparing', 'selectedFileIndex': 0,
        'files': [{'index': 0, 'name': letter + '.mkv', 'size': 100}],
        'progress': {'fileIndex': 0, 'stage': 'preparing', 'downloadedBytes': 50,
                     'totalBytes': 100, 'receivedBytes': 50, 'peers': 1,
                     'preparedSeconds': 2, 'sampledAt': '2026-10-18T00:00:00Z'},
    }


def test_progress(browser, base):
    state = {'downloads': [download('a'), download('b')], 'requests': 0}
    errors = []

    def api(route):
        path = route.request.url.split('/api/', 1)[1].split('?', 1)[0]
        data = {}
        if path == 'downloads':
            state['requests'] += 1
            data = state['downloads']
        elif path == 'auth/status':
            data = {'enabled': False}
        elif path == 'media/info':
            data = {'audioTracks': [{'index': 0}, {'index': 1}], 'subtitles': []}
        route.fulfill(content_type='application/json', body=json.dumps(data))

    context = browser.new_context()
    context.route('**/api/**', api)
    context.route('https://cdn.jsdelivr.net/**', lambda route: route.abort())
    page = context.new_page()
    page.clock.install()
    page.on('pageerror', lambda error: errors.append(str(error)))
    page.add_init_script('''window.EventSource = class extends EventTarget {
      static OPEN = 1;
      constructor() {
        super(); this.readyState = 1; window.testEvents = this;
        queueMicrotask(() => this.dispatchEvent(new Event('open')));
      }
      close() { this.readyState = 2; }
    };''')
    page.goto(base)
    rows = page.locator('.download-row')
    row_b = page.locator('.download-open').filter(has_text='B.mkv')
    expect(rows).to_have_count(2)
    row_b.focus()
    state['downloads'].reverse()
    page.evaluate("testEvents.dispatchEvent(new Event('changed'))")
    expect(rows.first).to_have_attribute('data-id', 'b' * 40)
    expect(row_b).to_be_focused()

    page.locator('#magnet').focus()
    state['downloads'].reverse()
    page.evaluate("testEvents.dispatchEvent(new Event('changed'))")
    expect(rows.first).to_have_attribute('data-id', 'a' * 40)
    expect(page.locator('#magnet')).to_be_focused()
    row_b.focus()
    state['downloads'].pop()
    page.evaluate("testEvents.dispatchEvent(new Event('changed'))")
    expect(rows).to_have_count(1)
    expect(page.locator('body')).to_be_focused()

    page.locator('.download-open').click()
    page.locator('#selectTracks').click()
    expect(page.locator('#step-tracks')).to_be_visible()
    current = state['downloads'][0]
    current.update(status='failed', preparationError='ffmpeg failed')
    current.pop('progress')
    page.evaluate("testEvents.dispatchEvent(new Event('changed'))")
    expect(page.locator('#preparationStage')).to_have_text('Preparation failed')
    expect(page.locator('#preparationDetails')).to_have_text('ffmpeg failed')
    expect(page.locator('#warnMsg')).not_to_contain_text('Preparation continues')
    current.update(status='ready', preparationError='')
    page.evaluate("testEvents.dispatchEvent(new Event('changed'))")
    expect(page.locator('#preparationStage')).to_have_text('Video is ready to play')
    expect(page.locator('#warnMsg')).not_to_contain_text('Preparation continues')

    count = state['requests']
    page.clock.fast_forward(2_001)
    assert state['requests'] == count, 'An open event stream should not poll completed downloads'
    page.evaluate("testEvents.readyState = 0; testEvents.dispatchEvent(new Event('error'))")
    current['title'] = 'First fallback'
    with page.expect_response('**/api/downloads?progress=1'):
        page.clock.fast_forward(5_001)
    expect(page.locator('.download-title')).to_have_text('First fallback')
    page.evaluate('testEvents.close()')
    current['title'] = 'Periodic fallback'
    with page.expect_response('**/api/downloads'):
        page.clock.fast_forward(30_001)
    expect(page.locator('.download-title')).to_have_text('Periodic fallback')
    page.evaluate("testEvents.readyState = EventSource.OPEN; testEvents.dispatchEvent(new Event('open'))")
    count = state['requests']
    page.clock.fast_forward(30_001)
    assert state['requests'] == count, 'Reconnecting should stop fallback polling'
    page.close()

    page = context.new_page()
    page.clock.install()
    page.on('pageerror', lambda error: errors.append(str(error)))
    page.add_init_script('window.EventSource = undefined')
    page.goto(base)
    expect(page.locator('.download-title')).to_have_text('Periodic fallback')
    current['title'] = 'No EventSource'
    with page.expect_response('**/api/downloads'):
        page.clock.fast_forward(30_001)
    expect(page.locator('.download-title')).to_have_text('No EventSource')
    assert not errors, errors
    context.close()


if __name__ == '__main__':
    with ThreadingHTTPServer(('127.0.0.1', 0), Handler) as server, sync_playwright() as playwright:
        threading.Thread(target=server.serve_forever, daemon=True).start()
        try:
            for engine in ('chromium', 'webkit'):
                browser = getattr(playwright, engine).launch()
                try:
                    test_progress(browser, f'http://127.0.0.1:{server.server_port}')
                    print(f'{engine}: progress regressions passed', flush=True)
                finally:
                    browser.close()
        finally:
            server.shutdown()
