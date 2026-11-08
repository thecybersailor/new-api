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
import { z } from 'zod'

import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

export const operations = [
  'text_generation',
  'image_generation',
  'image_edit',
  'video_generation',
  'audio_transcription',
  'speech_synthesis',
  'embedding',
  'multimodal_generation',
] as const
export type Operation = (typeof operations)[number]
export interface Definition {
  operation: Operation
  operationRevision: string
  pricingRevision: string
  inputSchema: Record<string, unknown>
  outputSchema: Record<string, unknown>
  limits: Record<string, unknown>
  usageSchema: Record<string, unknown>
  capabilities?: Record<string, unknown>
  inputModalities: string[]
  outputModalities: string[]
  executionMode: string[]
  protocol: string
  submitPath: string
  queryPathTemplate?: string
}
export interface RelayConfig {
  model?: string
  channelId?: number
  definition?: Partial<Definition>
  maxInputTokens?: number
  maxOutputTokens?: number
  usageBounds?: Record<string, { min: number; max: number }>
}
export interface CatalogOperation {
  operation: Operation
  config: RelayConfig
  evidence: string
  state: string
  dependencyHash?: string
}
export interface CatalogEntry {
  model: string
  displayName: string
  owner: string
  operations: CatalogOperation[]
}
export interface Issue {
  model: string
  operation: Operation
  field: string
  message: string
}
export interface Audit {
  id: number
  actorId: number
  action: string
  reason: string
  createdAt: string
  draftRevision: number
  publishedRevision: number
}
export interface Directory {
  draftRevision: number
  publishedRevision: number
  databaseEnabled: boolean
  entries: CatalogEntry[]
  published: CatalogEntry[]
  issues: Issue[]
  audit: Audit[]
  manifestEntries: CatalogEntry[]
  manifestRevision: string
  manifestError: string
  baseline: CatalogEntry[]
}
export interface Selection {
  model: string
  operation: Operation
}
export interface Mutation {
  draftRevision: number
  publishedRevision: number
  reason: string
  entries?: CatalogEntry[]
  selections?: Selection[]
  activateDatabase?: boolean
  manifestRevision?: string
}
export interface Binding {
  scopeId?: string
  bindingVersion?: string
  ID?: number
  tokenId: number
  userId: number
  principalId: string
  fixedGroup: string
  enabled: boolean
  revision: number
  modelPolicy: string
  allowedModels: string
  allowedOperations: string
}
interface Response<T> {
  success: boolean
  message?: string
  data: T
}
export const directoryKey = ['catalog-maintenance']
export async function getDirectory(): Promise<Directory> {
  const r = await api.get<Response<Directory>>('/api/catalog-maintenance')
  return requireServerSuccess(r.data).data
}
export async function saveDraft(body: Mutation): Promise<void> {
  const r = await api.put<Response<void>>(
    '/api/catalog-maintenance/draft',
    body
  )
  requireServerSuccess(r.data)
}
export async function changePublication(
  action: 'publish' | 'retire',
  body: Mutation
): Promise<void> {
  const r = await api.post<Response<void>>(
    `/api/catalog-maintenance/${action}`,
    body
  )
  requireServerSuccess(r.data)
}
export async function previewDirectory(
  body: Mutation
): Promise<
  Pick<
    Directory,
    'entries' | 'published' | 'issues' | 'draftRevision' | 'publishedRevision'
  >
> {
  const r = await api.post<Response<Directory>>(
    '/api/catalog-maintenance/preview',
    body
  )
  return requireServerSuccess(r.data).data
}
export async function getBindings(): Promise<Binding[]> {
  const r = await api.get<Response<Binding[]>>(
    '/api/catalog-maintenance/bindings'
  )
  return requireServerSuccess(r.data).data ?? []
}
export async function saveBinding(
  binding: Binding,
  reason: string
): Promise<void> {
  const r = await api.put<Response<void>>('/api/catalog-maintenance/bindings', {
    binding,
    reason,
  })
  requireServerSuccess(r.data)
}

const record = z.record(z.string(), z.unknown())
const importSchema = z
  .array(
    z.object({
      model: z.string().min(1).max(256),
      displayName: z.string().default(''),
      owner: z.string().default(''),
      operations: z.array(
        z.object({
          operation: z.enum(operations),
          config: record.default({}),
          evidence: z.string().default(''),
          state: z.string().default('draft'),
        })
      ),
    })
  )
  .max(500)
export function parseCatalogImport(raw: string): CatalogEntry[] {
  let parsed: unknown = JSON.parse(raw)
  const legacySchema = z.array(
    z
      .object({
        model: z.string().min(1),
        channelId: z.number().int().nonnegative(),
        definition: z.object({ operation: z.enum(operations) }).passthrough(),
      })
      .passthrough()
  )
  const legacy = legacySchema.safeParse(parsed)
  if (legacy.success) {
    const grouped: CatalogEntry[] = []
    for (const config of legacy.data) {
      const entry = grouped.find((item) => item.model === config.model) ?? {
        model: config.model,
        displayName: config.model,
        owner: '',
        operations: [],
      }
      if (!grouped.includes(entry)) {
        grouped.push(entry)
      }
      entry.operations.push({
        operation: config.definition.operation,
        config: config as RelayConfig,
        evidence: '',
        state: 'draft',
      })
    }
    parsed = grouped
  }
  return importSchema.parse(parsed) as CatalogEntry[]
}
export function operationKey(model: string, operation: string): string {
  return `${model}/${operation}`
}
export function mergeCatalogCandidates(
  current: CatalogEntry[],
  incoming: CatalogEntry[]
): CatalogEntry[] {
  const out = structuredClone(current)
  for (const entry of incoming) {
    const previous = out.find((item) => item.model === entry.model)
    if (!previous) {
      out.push(entry)
      continue
    }
    for (const op of entry.operations) {
      const index = previous.operations.findIndex(
        (item) => item.operation === op.operation
      )
      if (index < 0) previous.operations.push(op)
      else previous.operations[index] = op
    }
    if (entry.owner) previous.owner = entry.owner
    if (entry.displayName) previous.displayName = entry.displayName
  }
  return out
}
