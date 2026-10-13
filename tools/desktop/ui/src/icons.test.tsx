/// <reference types="node" />
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'

// icons.tsx is a COPY of web/src/icons.tsx (this bundle cannot import web/).
// Reading both sources as text — not importing web/ — pins each copied glyph
// to its original, so the sidebar cannot drift from the knowledge app.
const blocks = (src: string) =>
  new Map(Array.from(src.matchAll(/export const (\w+) ?=[\s\S]*?\n\);/g), (m) => [m[1], m[0]]))

const here = blocks(readFileSync(resolve(__dirname, 'icons.tsx'), 'utf8'))
const web = blocks(readFileSync(resolve(__dirname, '../../../../web/src/icons.tsx'), 'utf8'))

describe('desktop icons', () => {
  it('copies the sidebar and copy glyphs', () => {
    expect([...here.keys()]).toEqual(['GearIcon', 'GlobeIcon', 'BroadcastIcon', 'FolderIcon', 'CopyIcon'])
  })

  it.each([...here.keys()])('%s is byte-identical to web/src/icons.tsx', (name) => {
    expect(web.get(name)).toBeDefined()
    expect(here.get(name)).toBe(web.get(name))
  })
})
