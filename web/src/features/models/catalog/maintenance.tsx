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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Field, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Textarea } from '@/components/ui/textarea'
import { hasPermission } from '@/lib/admin-permissions'
import { formatNumber } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import {
  changePublication,
  directoryKey,
  getDirectory,
  mergeCatalogCandidates,
  operationKey,
  parseCatalogImport,
  previewDirectory,
  saveDraft,
  type CatalogEntry,
  type CatalogOperation,
  type Directory,
  type Selection,
} from './api'
import { CatalogBindings } from './bindings'
import { OperationEditor } from './operation-editor'

type Row = { entry: CatalogEntry; op: CatalogOperation }
type Preview = {
  base: Directory
  action: 'publish' | 'retire'
  selections: Selection[]
  review: Awaited<ReturnType<typeof previewDirectory>>
}

export function CatalogMaintenance() {
  const { t } = useTranslation()
  const client = useQueryClient()
  const user = useAuthStore((s) => s.auth.user)
  const canEdit = hasPermission(user, 'catalog', 'edit')
  const canPublish = hasPermission(user, 'catalog', 'publish')
  const query = useQuery({ queryKey: directoryKey, queryFn: getDirectory })
  const [filter, setFilter] = useState('')
  const [selected, setSelected] = useState<string[]>([])
  const [editing, setEditing] = useState<{
    base: Directory
    row?: Row
    entry?: CatalogEntry
  }>()
  const [preview, setPreview] = useState<Preview>()
  const [reason, setReason] = useState('')
  const [importing, setImporting] = useState<{
    base: Directory
    text: string
    entries?: CatalogEntry[]
  }>()
  const mutation = useMutation({
    mutationFn: async (job: () => Promise<void>) => job(),
    onError: (e) => handleServerError(e),
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: directoryKey })
    },
  })
  const data = query.data
  if (query.isPending) return <LoadingState />
  if (query.isError || !data) {
    return <ErrorState onRetry={() => void query.refetch()} />
  }
  const rows = data.entries
    .flatMap((entry) => entry.operations.map((op) => ({ entry, op })))
    .filter((row) =>
      `${row.entry.model} ${row.op.operation} ${row.entry.owner}`
        .toLowerCase()
        .includes(filter.toLowerCase())
    )
  const selectedRows = data.entries.flatMap((entry) =>
    entry.operations
      .filter((op) =>
        selected.includes(operationKey(entry.model, op.operation))
      )
      .map((op) => ({ model: entry.model, operation: op.operation }))
  )
  const persist = async (
    base: Directory,
    entries: CatalogEntry[],
    message: string
  ) => {
    await mutation.mutateAsync(() =>
      saveDraft({
        draftRevision: base.draftRevision,
        publishedRevision: base.publishedRevision,
        entries,
        reason: message,
      })
    )
  }
  const prepare = async (action: 'publish' | 'retire') => {
    try {
      const review = await previewDirectory({
        draftRevision: data.draftRevision,
        publishedRevision: data.publishedRevision,
        reason: '',
      })
      setPreview({ base: data, action, selections: selectedRows, review })
      setReason('')
    } catch (e) {
      handleServerError(e)
    }
  }
  const blocking =
    preview?.review.issues.filter((issue) =>
      preview.selections.some(
        (item) =>
          item.model === issue.model && item.operation === issue.operation
      )
    ) ?? []
  return (
    <div
      className='flex h-full flex-col gap-4 overflow-auto pb-8 [&>*]:shrink-0'
      data-testid='catalog-maintenance'
    >
      {data.manifestError && <p role='alert'>{t(data.manifestError)}</p>}
      <p className='text-muted-foreground text-sm'>
        {t(
          'Supplier publication and consumer admission are separate. Real model verification is still required.'
        )}
      </p>
      <p>
        {t('Models')}: {formatNumber(data.entries.length)} ·{' '}
        {t('Published revision')}: {formatNumber(data.publishedRevision)} ·{' '}
        {t('Draft revision')}: {formatNumber(data.draftRevision)}
      </p>
      <div className='flex flex-wrap gap-2'>
        <Input
          aria-label={t('Filter catalog operations')}
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          className='max-w-sm'
          placeholder={t('Search model, operation or owner')}
        />
        <Button variant='outline' onClick={() => void query.refetch()}>
          {t('Refresh')}
        </Button>
        {canEdit && (
          <>
            <Button
              disabled={mutation.isPending}
              onClick={() => setEditing({ base: data })}
            >
              {t('Add operation')}
            </Button>
            <Button
              variant='outline'
              onClick={() =>
                setImporting({
                  base: data,
                  text: JSON.stringify(data.baseline, null, 2),
                })
              }
            >
              {t('Import 25 candidates')}
            </Button>
            <Button
              variant='outline'
              onClick={() => setImporting({ base: data, text: '' })}
            >
              {t('Import configuration')}
            </Button>
            {!!data.manifestEntries?.length && (
              <Button
                variant='outline'
                onClick={() =>
                  setImporting({
                    base: data,
                    text: JSON.stringify(data.manifestEntries, null, 2),
                  })
                }
              >
                {t('Import server manifest')}
              </Button>
            )}
          </>
        )}
        <CopyButton
          value={JSON.stringify(data.entries, null, 2)}
          tooltip={t('Copy catalog configuration')}
        />
        {canPublish && (
          <>
            <Button
              disabled={!selectedRows.length || mutation.isPending}
              onClick={() => void prepare('publish')}
            >
              {t('Preview publication')}
            </Button>
            <Button
              variant='outline'
              disabled={!selectedRows.length || mutation.isPending}
              onClick={() => void prepare('retire')}
            >
              {t('Retire selected operations')}
            </Button>
          </>
        )}
      </div>
      <StaticDataTable<Row>
        data={rows}
        getRowKey={(r) => operationKey(r.entry.model, r.op.operation)}
        emptyContent={t('Import candidates or add an operation to begin.')}
        columns={[
          {
            id: 'select',
            header: t('Select'),
            cell: (r) => {
              const key = operationKey(r.entry.model, r.op.operation)
              return (
                <Checkbox
                  aria-label={`${t('Select')} ${key}`}
                  checked={selected.includes(key)}
                  onCheckedChange={(checked) =>
                    setSelected((old) =>
                      checked ? [...old, key] : old.filter((v) => v !== key)
                    )
                  }
                />
              )
            },
          },
          {
            id: 'model',
            header: t('Model ID'),
            cell: (r) => (
              <div>
                <strong>{r.entry.model}</strong>
                <p>{r.entry.displayName}</p>
              </div>
            ),
          },
          {
            id: 'operation',
            header: t('Operation'),
            cell: (r) => r.op.operation,
          },
          {
            id: 'owner',
            header: t('Configuration owner'),
            cell: (r) => r.entry.owner || t('Unassigned'),
          },
          {
            id: 'status',
            header: t('Supplier status'),
            cell: (r) => {
              const live = data.published
                .find((item) => item.model === r.entry.model)
                ?.operations.find((op) => op.operation === r.op.operation)
              if (live?.state === 'published') return t('Published')
              if (live?.state === 'retired') return t('Retired')
              return t('Draft')
            },
          },
          {
            id: 'issues',
            header: t('Configuration issues'),
            cell: (r) => (
              <ul>
                {data.issues
                  .filter(
                    (issue) =>
                      issue.model === r.entry.model &&
                      issue.operation === r.op.operation
                  )
                  .map((issue) => (
                    <li key={issue.field}>
                      {t(issue.field)}: {t(issue.message)}
                    </li>
                  ))}
              </ul>
            ),
          },
          {
            id: 'consumer',
            header: t('Consumer admission'),
            cell: () => t('Review in the digital employee platform'),
          },
          {
            id: 'actions',
            header: t('Actions'),
            cell: (r) =>
              canEdit && (
                <div className='flex gap-2'>
                  <Button
                    variant='outline'
                    onClick={() => setEditing({ base: data, row: r })}
                  >
                    {t('Edit')}
                  </Button>
                  <Button
                    variant='outline'
                    onClick={() => setEditing({ base: data, entry: r.entry })}
                  >
                    {t('Add operation')}
                  </Button>
                  {!data.published.some(
                    (item) =>
                      item.model === r.entry.model &&
                      item.operations.some(
                        (op) => op.operation === r.op.operation
                      )
                  ) && (
                    <Button
                      variant='ghost'
                      disabled={mutation.isPending}
                      onClick={() => {
                        const next = structuredClone(data.entries)
                        const entry = next.find(
                          (item) => item.model === r.entry.model
                        )
                        if (entry) {
                          entry.operations = entry.operations.filter(
                            (op) => op.operation !== r.op.operation
                          )
                        }
                        void persist(
                          data,
                          next.filter((item) => item.operations.length),
                          'Delete unpublished candidate'
                        ).catch(() => undefined)
                      }}
                    >
                      {t('Delete draft')}
                    </Button>
                  )}
                </div>
              ),
          },
        ]}
      />
      <h3 className='font-semibold'>{t('Publication audit')}</h3>
      <StaticDataTable
        data={data.audit ?? []}
        getRowKey={(row) => row.id}
        columns={[
          { id: 'action', header: t('Action'), cell: (row) => row.action },
          { id: 'reason', header: t('Reason'), cell: (row) => row.reason },
          {
            id: 'actor',
            header: t('Actor'),
            cell: (row) => formatNumber(row.actorId),
          },
          { id: 'time', header: t('Time'), cell: (row) => row.createdAt },
        ]}
      />
      {hasPermission(user, 'catalog', 'authorize') && <CatalogBindings />}
      {editing && (
        <OperationEditor
          entry={editing.row?.entry ?? editing.entry}
          operation={editing.row?.op}
          pending={mutation.isPending}
          onClose={() => setEditing(undefined)}
          onSave={async (entry) => {
            if (
              !editing.row &&
              editing.base.entries.some(
                (old) =>
                  old.model === entry.model &&
                  old.operations.some(
                    (op) => op.operation === entry.operations[0].operation
                  )
              )
            ) {
              handleServerError(
                new Error(t('This operation already exists. Edit it instead.'))
              )
              return
            }
            try {
              await persist(
                editing.base,
                mergeCatalogCandidates(editing.base.entries, [entry]),
                'Update operation configuration'
              )
              setEditing(undefined)
            } catch {
              /* preserve draft after conflict */
            }
          }}
        />
      )}
      {preview && (
        <ConfirmDialog
          open
          onOpenChange={(open) => {
            if (!open && !mutation.isPending) setPreview(undefined)
          }}
          title={t(
            preview.action === 'publish'
              ? 'Confirm supplier publication'
              : 'Confirm retirement'
          )}
          desc={t(
            preview.action === 'publish'
              ? 'Publishing changes the supplier catalog only. New operations still require consumer admission.'
              : 'Stop new calls for the selected operations. Keep existing calls, assets and billing history.'
          )}
          destructive={preview.action === 'retire'}
          isLoading={mutation.isPending}
          disabled={
            !reason.trim() ||
            (preview.action === 'publish' && blocking.length > 0)
          }
          handleConfirm={() => {
            void mutation
              .mutateAsync(() =>
                changePublication(preview.action, {
                  draftRevision: preview.review.draftRevision,
                  publishedRevision: preview.review.publishedRevision,
                  selections: preview.selections,
                  reason,
                  activateDatabase: !preview.base.databaseEnabled,
                  manifestRevision: preview.base.manifestRevision,
                })
              )
              .then(() => {
                setPreview(undefined)
                setSelected([])
              })
              .catch(() => undefined)
          }}
        >
          {!preview.base.databaseEnabled && (
            <p>
              {t(
                'This first publication switches execution to the database catalog. Configuration files will no longer be read.'
              )}
            </p>
          )}
          <ul>
            {preview.selections.map((s) => (
              <li key={operationKey(s.model, s.operation)}>
                {s.model} · {s.operation}
              </li>
            ))}
          </ul>
          <details>
            <summary>{t('Review exact configuration changes')}</summary>
            <pre className='max-h-64 overflow-auto text-xs'>
              {JSON.stringify(
                {
                  before: preview.review.published.filter((entry) =>
                    preview.selections.some((s) => s.model === entry.model)
                  ),
                  after: preview.review.entries.filter((entry) =>
                    preview.selections.some((s) => s.model === entry.model)
                  ),
                },
                null,
                2
              )}
            </pre>
          </details>
          {blocking.map((issue) => (
            <p
              key={`${issue.model}/${issue.operation}/${issue.field}`}
              role='alert'
            >
              {issue.model}: {issue.field} — {t(issue.message)}
            </p>
          ))}
          <Field>
            <FieldLabel htmlFor='catalog-change-reason'>
              {t('Change reason')}
            </FieldLabel>
            <Textarea
              id='catalog-change-reason'
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
          </Field>
        </ConfirmDialog>
      )}
      {importing && (
        <Dialog
          open
          title={t('Import configuration preview')}
          onOpenChange={(open) => {
            if (!open && !mutation.isPending) setImporting(undefined)
          }}
          description={t(
            'Import creates drafts only. Existing published operations cannot be removed by importing.'
          )}
          footer={
            <>
              <Button
                variant='outline'
                onClick={() => {
                  try {
                    setImporting({
                      ...importing,
                      entries: parseCatalogImport(importing.text),
                    })
                  } catch (e) {
                    handleServerError(e)
                  }
                }}
              >
                {t('Validate import')}
              </Button>
              <Button
                disabled={!importing.entries || mutation.isPending}
                onClick={() => {
                  if (importing.entries) {
                    void persist(
                      importing.base,
                      mergeCatalogCandidates(
                        importing.base.entries,
                        importing.entries
                      ),
                      'Import reviewed candidates'
                    )
                      .then(() => setImporting(undefined))
                      .catch(() => undefined)
                  }
                }}
              >
                {t('Save imported drafts')}
              </Button>
            </>
          }
        >
          <Textarea
            aria-label={t('Catalog configuration JSON')}
            rows={12}
            value={importing.text}
            onChange={(e) =>
              setImporting({
                ...importing,
                text: e.target.value,
                entries: undefined,
              })
            }
          />
          {importing.entries && (
            <ul>
              {importing.entries.map((entry) => (
                <li key={entry.model}>
                  {entry.model} · {formatNumber(entry.operations.length)}{' '}
                  {t('operations')}
                </li>
              ))}
            </ul>
          )}
        </Dialog>
      )}
    </div>
  )
}
