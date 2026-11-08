import { beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { exchangeDanQiuOSCallback } from './danqiuos'
import { hasOAuthProviders } from './oauth'

vi.mock('@/lib/api', () => ({ api: { post: vi.fn() } }))

describe('DanQiuOS session transfer', () => {
  beforeEach(() => {
    vi.resetAllMocks()
  })
  it('clears fragment first and sends only the access token in a one-use POST', async () => {
    const replaceState = vi.fn()
    vi.mocked(api.post).mockImplementation(async () => {
      expect(replaceState).toHaveBeenCalledWith(null, '', '/auth/callback')
      return { data: { success: true, data: { code: 'local-assertion' } } }
    })
    const transfer = {
      user: { id: 'untrusted', role: 'admin' },
      session: {
        access_token: 'central-access',
        refresh_token: 'never-forward-this',
      },
    }
    const result = await exchangeDanQiuOSCallback(
      {
        pathname: '/auth/callback',
        search: '?state=flow-state',
        hash: `#account_session=${encodeURIComponent(JSON.stringify(transfer))}`,
      },
      { replaceState }
    )
    expect(result).toBe('/oauth/danqiuos?state=flow-state&code=local-assertion')
    expect(api.post).toHaveBeenCalledExactlyOnceWith(
      '/api/danqiuos/assertion',
      { state: 'flow-state', access_token: 'central-access' },
      expect.objectContaining({
        skipAuthRefresh: true,
      })
    )
    expect(JSON.stringify(vi.mocked(api.post).mock.calls)).not.toContain(
      'never-forward-this'
    )
  })
  it.each(['', '#account_session=broken', '#account_session=%7B%7D'])(
    'rejects invalid transfer %s without sending a request',
    async (hash) => {
      const replaceState = vi.fn()
      await expect(
        exchangeDanQiuOSCallback(
          { pathname: '/auth/callback', search: '?state=s', hash },
          { replaceState }
        )
      ).rejects.toThrow()
      expect(replaceState).toHaveBeenCalledOnce()
      expect(api.post).not.toHaveBeenCalled()
    }
  )
  it('stops on a failed assertion without retry or another login method', async () => {
    vi.mocked(api.post).mockResolvedValue({
      data: { success: false, message: 'OAuth failed' },
    })
    await expect(
      exchangeDanQiuOSCallback(
        {
          pathname: '/auth/callback',
          search: '?state=s',
          hash: `#account_session=${encodeURIComponent(
            JSON.stringify({ session: { access_token: 'invalid' } })
          )}`,
        },
        { replaceState: vi.fn() }
      )
    ).rejects.toThrow()
    expect(api.post).toHaveBeenCalledOnce()
  })
  it('shows the existing provider area when DanQiuOS is the only login provider', () => {
    expect(hasOAuthProviders({ danqiuos_login: true })).toBe(true)
  })
})

it('captures the fragment before router startup and exchanges only once across repeated loaders', async () => {
  const module = await import('./danqiuos')
  const replaceState = vi.fn()
  vi.mocked(api.post).mockResolvedValue({
    data: { success: true, data: { code: 'one-exchange' } },
  })
  module.captureDanQiuOSCallback(
    {
      pathname: '/auth/callback',
      search: '?state=initial',
      hash: `#account_session=${encodeURIComponent(JSON.stringify({ session: { access_token: 'central' } }))}`,
    },
    { replaceState }
  )
  expect(replaceState).toHaveBeenCalledExactlyOnceWith(
    null,
    '',
    '/auth/callback'
  )
  const first = module.exchangeCapturedDanQiuOSCallback()
  const second = module.exchangeCapturedDanQiuOSCallback()
  expect(first).toBe(second)
  expect(await first).toBe('/oauth/danqiuos?state=initial&code=one-exchange')
  expect(api.post).toHaveBeenCalledOnce()
})
