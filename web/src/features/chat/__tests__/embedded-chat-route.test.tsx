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
import { render, screen, cleanup } from '@testing-library/react'
import { afterEach, expect, it, vi } from 'vitest'

import { Route } from '@/routes/_authenticated/chat/$chatId'

const state = vi.hoisted(() => ({
  url: 'https://chat.example/?key={key}',
  enabled: vi.fn(),
  key: 'fixture-key' as string | undefined,
}))
vi.mock('@tanstack/react-router', () => ({
  createFileRoute: () => (options: object) => ({
    options,
    useParams: () => ({ chatId: '0' }),
  }),
  Link: 'a',
  redirect: vi.fn(),
}))
vi.mock('@/features/chat/hooks/use-chat-presets', () => ({
  useChatPresets: () => ({
    chatPresets: [{ type: 'web', name: 'Fixture', url: state.url }],
    serverAddress: 'https://api.example',
  }),
}))
vi.mock('@/features/chat/hooks/use-active-chat-key', () => ({
  useActiveChatKey: (enabled: boolean) => {
    state.enabled(enabled)
    return { data: state.key, isPending: false, isError: false }
  },
}))

const Component = Route.options.component as () => React.ReactNode
afterEach(() => {
  cleanup()
  state.enabled.mockClear()
  state.key = 'fixture-key'
})
it('rejects same-origin presets before requesting the active key', () => {
  state.url = `${window.location.origin}/?key={key}`
  const { container } = render(<Component />)
  expect(state.enabled).toHaveBeenCalledWith(false)
  expect(container.querySelector('iframe')).toBeNull()
  expect(screen.getByText('Unable to open chat')).toBeVisible()
  expect(
    screen.getByText(
      'Ask an administrator to set a cross-origin HTTP(S) chat URL without a username or password.'
    )
  ).toBeVisible()
})
it('preserves the missing-key gate for valid cross-origin presets', () => {
  state.url = 'https://chat.example/?key={key}'
  state.key = undefined
  const { container } = render(<Component />)
  expect(state.enabled).toHaveBeenCalledWith(true)
  expect(container.querySelector('iframe')).toBeNull()
})
it('renders a cross-origin app with the approved key and sandbox', () => {
  state.url = 'https://chat.example/?key={key}'
  const { container } = render(<Component />)
  expect(state.enabled).toHaveBeenCalledWith(true)
  const frame = container.querySelector('iframe')
  expect(frame).toHaveAttribute(
    'src',
    'https://chat.example/?key=sk-fixture-key'
  )
  expect(frame?.getAttribute('sandbox')).toContain('allow-same-origin')
})
