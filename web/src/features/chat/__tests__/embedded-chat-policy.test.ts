/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { describe, it, expect } from 'vitest'

import { chatFrameSandbox } from '../lib/embedded-chat-policy'

describe('embedded chat origin gate', () => {
  it('preserves scripts, forms and storage only for cross-origin apps without parent navigation', () => {
    const tokens = chatFrameSandbox(
      'https://chat.example/app?key=fixture',
      'https://dantoken.cn'
    ).split(' ')
    expect(tokens).toContain('allow-scripts')
    expect(tokens).toContain('allow-same-origin')
    expect(tokens).toContain('allow-forms')
    expect(tokens).not.toContain('allow-top-navigation')
    expect(tokens).not.toContain('allow-popups-to-escape-sandbox')
  })
  it.each([
    'https://dantoken.cn/chat',
    'https://dantoken.cn:443/chat',
    '/chat',
    'javascript:alert(1)',
    'data:text/html,test',
    'https://user@chat.example/',
  ])('rejects %s without an unsandboxed path', (target) => {
    expect(() => chatFrameSandbox(target, 'https://dantoken.cn')).toThrow()
  })
})
