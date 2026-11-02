#!/usr/bin/env python3
"""Extract the supplied dashboard bundle and optionally capture its six boards.

The output is generated review evidence, not application code. Keep the original
bundle beside exported copies: it contains the bundled fonts' license notices.
"""
import argparse
import base64
import gzip
import hashlib
import json
from pathlib import Path
import re
import subprocess


def island(document, name):
    match = re.search(r'<script type="__bundler/' + name + r'">(.*?)</script>', document, re.S)
    if match is None:
        raise ValueError('Missing bundle island: ' + name)
    return json.loads(match.group(1))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('bundle', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--puppeteer', type=Path, help='Installed puppeteer or puppeteer-core module directory')
    parser.add_argument('--chrome', type=Path, default=Path('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'))
    args = parser.parse_args()
    source = args.bundle.resolve()
    document = source.read_text()
    manifest = island(document, 'manifest')
    order = island(document, 'page_order')
    template = island(document, 'template')
    frames = re.findall(r'<iframe[^>]*title="([^"]+)"[^>]*width="(\d+)"[^>]*height="(\d+)"', template)
    names = ('desktop', 'tablet', 'mobile', 'room', 'agenda', 'tokens')
    if len(order) != len(frames) or len(order) != len(names):
        raise ValueError('Expected the supplied six-board dashboard bundle')
    args.output.mkdir(parents=True, exist_ok=True)
    boards = []
    for key, name, (title, width, height) in zip(order, names, frames):
        entry = manifest[key]
        payload = base64.b64decode(entry['data'])
        if entry.get('compressed'):
            payload = gzip.decompress(payload)
        nested = payload.decode()
        html = island(nested, 'template')
        (args.output / (name + '.source.html')).write_text(html)
        boards.append(dict(name=name, title=title, width=int(width), height=int(height)))
    record = dict(bundle=str(source), sha256=hashlib.sha256(source.read_bytes()).hexdigest(), boards=boards)
    (args.output / 'reference.json').write_text(json.dumps(record, indent=2) + '\n')
    if args.puppeteer:
        # Pass values as process arguments; source paths never become shell code.
        runner = r'''
const fs = require('fs');
const puppeteer = require(process.argv[1]);
(async () => {
  const browser = await puppeteer.launch({executablePath: process.argv[2], headless: true, args: ['--allow-file-access-from-files']});
  try {
    const page = await browser.newPage();
    await page.setViewport({width: 1550, height: 1100, deviceScaleFactor: 1});
    await page.goto(process.argv[3], {waitUntil: 'networkidle0', timeout: 60000});
    await new Promise(resolve => setTimeout(resolve, 1000));
    const frames = page.frames().slice(1);
    const boards = JSON.parse(process.argv[5]);
    if (frames.length !== boards.length) throw new Error('Missing rendered frames');
    for (let i = 0; i < boards.length; i++) {
      const frame = frames[i];
      await (await frame.frameElement()).screenshot({path: process.argv[4] + '/' + boards[i].name + '.png'});
      fs.writeFileSync(process.argv[4] + '/' + boards[i].name + '.txt', await frame.evaluate(() => document.body.innerText));
    }
  } finally { await browser.close(); }
})().catch(error => { console.error(error); process.exit(1); });
'''
        subprocess.run(['node', '-e', runner, str(args.puppeteer.resolve()), str(args.chrome), source.as_uri(),
                        str(args.output.resolve()), json.dumps(boards)], check=True)
    print(args.output.resolve())


if __name__ == '__main__':
    main()
