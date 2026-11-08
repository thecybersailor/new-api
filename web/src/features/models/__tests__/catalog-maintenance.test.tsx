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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import {
  mergeCatalogCandidates,
  parseCatalogImport,
  type Directory,
} from '../catalog/api'
import { CatalogMaintenance } from '../catalog/maintenance'

const base: Directory = {
  draftRevision: 3,
  publishedRevision: 1,
  databaseEnabled: true,
  entries: [
    {
      model: 'fixture/image',
      displayName: 'Fixture',
      owner: 'operator',
      operations: [
        {
          operation: 'image_generation',
          config: {},
          state: 'draft',
          evidence: 'local fixture only',
        },
      ],
    },
  ],
  published: [],
  issues: [],
  audit: [],
  baseline: [
    {
      model: 'candidate/text',
      displayName: 'Candidate',
      owner: '',
      operations: [
        {
          operation: 'text_generation',
          config: {},
          state: 'draft',
          evidence: '',
        },
      ],
    },
  ],
  manifestEntries: [],
  manifestRevision: '',
  manifestError: '',
}
function mount(data = base, edit = true) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'fixture',
    role: 10,
    permissions: {
      admin_permissions: {
        catalog: { read: true, edit, publish: edit, authorize: false },
      },
    },
  })
  vi.spyOn(api, 'get').mockResolvedValue({ data: { success: true, data } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <CatalogMaintenance />
    </QueryClientProvider>
  )
  return userEvent.setup()
}
afterEach(() => useAuthStore.getState().auth.reset())

describe('catalog maintenance', () => {
  it('keeps catalog and audit sections from collapsing in the scrollable page', async () => {
    mount()
    await screen.findByRole('checkbox', {
      name: 'Select fixture/image/image_generation',
    })
    expect(screen.getByTestId('catalog-maintenance')).toHaveClass(
      '[&>*]:shrink-0'
    )
  })
  it('imports candidates only after preview and never publishes them', async () => {
    const put = vi
      .spyOn(api, 'put')
      .mockResolvedValue({ data: { success: true } })
    const post = vi.spyOn(api, 'post')
    const user = mount()
    await user.click(
      await screen.findByRole('button', { name: 'Import 25 candidates' })
    )
    const dialog = screen.getByRole('dialog')
    expect(
      within(dialog).getByRole('button', { name: 'Save imported drafts' })
    ).toBeDisabled()
    await user.click(
      within(dialog).getByRole('button', { name: 'Validate import' })
    )
    await user.click(
      within(dialog).getByRole('button', { name: 'Save imported drafts' })
    )
    await waitFor(() => expect(put).toHaveBeenCalledOnce())
    expect(put.mock.calls[0][0]).toBe('/api/catalog-maintenance/draft')
    expect(put.mock.calls[0][1]).toMatchObject({
      draftRevision: 3,
      publishedRevision: 1,
      entries: [{ model: 'fixture/image' }, { model: 'candidate/text' }],
    })
    expect(post).not.toHaveBeenCalled()
  })
  it('publishes the reviewed exact operations and versions only after confirmation', async () => {
    const post = vi.spyOn(api, 'post').mockImplementation(async (path) => ({
      data: {
        success: true,
        data: path.endsWith('/preview') ? base : undefined,
      },
    }))
    const user = mount()
    await user.click(
      await screen.findByRole('checkbox', {
        name: 'Select fixture/image/image_generation',
      })
    )
    await user.click(
      screen.getByRole('button', { name: 'Preview publication' })
    )
    const dialog = await screen.findByRole('alertdialog')
    expect(post).toHaveBeenCalledTimes(1)
    expect(
      within(dialog).getByRole('button', { name: 'Continue' })
    ).toBeDisabled()
    await user.type(
      within(dialog).getByLabelText('Change reason'),
      'Reviewed fixture publication'
    )
    await user.click(within(dialog).getByRole('button', { name: 'Continue' }))
    await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
    expect(post.mock.calls[1]).toEqual([
      '/api/catalog-maintenance/publish',
      {
        draftRevision: 3,
        publishedRevision: 1,
        selections: [{ model: 'fixture/image', operation: 'image_generation' }],
        reason: 'Reviewed fixture publication',
        activateDatabase: false,
        manifestRevision: '',
      },
    ])
  })
  it('keeps the edited draft open when a version conflict rejects saving', async () => {
    vi.spyOn(api, 'put').mockRejectedValue(
      new Error('directory changed since preview')
    )
    const user = mount()
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    await user.clear(screen.getByLabelText('Configuration owner'))
    await user.type(screen.getByLabelText('Configuration owner'), 'new-owner')
    await user.click(screen.getByRole('button', { name: 'Save draft' }))
    await waitFor(() => expect(api.put).toHaveBeenCalledOnce())
    expect(screen.getByRole('dialog')).toBeInTheDocument()
    expect(screen.getByLabelText('Configuration owner')).toHaveValue(
      'new-owner'
    )
  })
  it('shows missing evidence and blocks publication while retirement remains possible', async () => {
    const data: Directory = {
      ...base,
      issues: [
        {
          model: 'fixture/image',
          operation: 'image_generation',
          field: 'evidence',
          message: 'Provide evidence',
        },
      ],
    }
    vi.spyOn(api, 'post').mockResolvedValue({ data: { success: true, data } })
    const user = mount(data)
    await user.click(
      await screen.findByRole('checkbox', {
        name: 'Select fixture/image/image_generation',
      })
    )
    await user.click(
      screen.getByRole('button', { name: 'Preview publication' })
    )
    const dialog = await screen.findByRole('alertdialog')
    await user.type(within(dialog).getByLabelText('Change reason'), 'review')
    expect(
      within(dialog).getByRole('button', { name: 'Continue' })
    ).toBeDisabled()
    expect(within(dialog).getByRole('alert')).toHaveTextContent(
      'Provide evidence'
    )
  })
  it('does not expose editing or publication to a read-only operator', async () => {
    mount(base, false)
    await screen.findByText('fixture/image')
    expect(
      screen.queryByRole('button', { name: 'Preview publication' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Edit' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('button', { name: 'Add integration binding' })
    ).not.toBeInTheDocument()
  })
  it('accepts legacy manifest import as drafts and preserves unrelated operations', () => {
    const incoming = parseCatalogImport(
      '[{"model":"fixture/image","channelId":9,"definition":{"operation":"image_edit"}}]'
    )
    const merged = mergeCatalogCandidates(base.entries, incoming)
    expect(merged[0].operations.map((op) => op.operation)).toEqual([
      'image_generation',
      'image_edit',
    ])
    expect(incoming[0].operations[0]).toMatchObject({
      state: 'draft',
      evidence: '',
    })
    expect(() =>
      parseCatalogImport(
        '[{"model":"bad","operations":[{"operation":"invented"}]}]'
      )
    ).toThrow()
  })
})
