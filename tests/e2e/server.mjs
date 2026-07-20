import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';

const clientDir = resolve('src/presentation/web/client/index');
const files = new Map([
  ['/', ['index.html', 'text/html; charset=utf-8']],
  ['/index.html', ['index.html', 'text/html; charset=utf-8']],
  ['/index.css', ['index.css', 'text/css; charset=utf-8']],
  ['/index.js', ['index.js', 'text/javascript; charset=utf-8']],
]);

const server = createServer(async (request, response) => {
  const entry = files.get(new URL(request.url, 'http://localhost').pathname);
  if (!entry) {
    response.writeHead(404).end();
    return;
  }
  try {
    const [name, contentType] = entry;
    let body = await readFile(resolve(clientDir, name));
    if (name === 'index.html') {
      body = Buffer.from(body.toString().replace(/\s+integrity="[^"]+"\s+crossorigin="anonymous"/, ''));
    }
    response.writeHead(200, { 'content-type': contentType }).end(body);
  } catch (error) {
    response.writeHead(500).end(String(error));
  }
});

server.listen(4173, '127.0.0.1');
process.on('SIGTERM', () => server.close());
