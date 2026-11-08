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
import { render, screen } from '@testing-library/react'
import { expect, it, vi } from 'vitest'

import { Footer } from '../footer'

vi.mock('@/hooks/use-system-config', () => ({
  useSystemConfig: () => ({
    footerHtml:
      '<a href="https://example.test/legal">Legal terms</a><strong>Company</strong><script>bad()</script><img src="/logo.png" onerror="bad()"><a href="javascript:bad()">Invalid link</a>',
  }),
}))
vi.mock('@/hooks/use-status', () => ({ useStatus: () => ({ data: {} }) }))
it('keeps legitimate custom footer content and attribution while removing executable HTML', () => {
  const { container } = render(<Footer />)
  expect(screen.getByRole('link', { name: 'Legal terms' })).toHaveAttribute(
    'href',
    'https://example.test/legal'
  )
  expect(screen.getByText('Company').tagName).toBe('STRONG')
  expect(screen.getByRole('link', { name: 'New API' })).toHaveAttribute(
    'href',
    'https://github.com/QuantumNous/new-api'
  )
  expect(container.querySelector('script')).toBeNull()
  expect(container.querySelector('[onerror]')).toBeNull()
  expect(screen.getByText('Invalid link')).not.toHaveAttribute('href')
})
