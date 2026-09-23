import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { formatRelativeTime } from '../../lib/format'
import { ConfirmDialog } from '../common/ConfirmDialog'
import type { EvolutionSuggestion } from '../../types/evolution'

interface EvolutionSuggestionsListProps {
  suggestions: EvolutionSuggestion[]
  loading: boolean
  onUpdateStatus: (id: string, status: 'approved' | 'rejected' | 'rolled_back') => Promise<void>
}

const TYPE_COLORS: Record<string, string> = {
  threshold: 'bg-blue-500/10 text-blue-600',
  tool_order: 'bg-orange-500/10 text-orange-600',
  skill_add: 'bg-green-500/10 text-green-600',
}

const STATUS_COLORS: Record<string, string> = {
  pending: 'bg-yellow-500/10 text-yellow-600',
  approved: 'bg-blue-500/10 text-blue-600',
  applying: 'bg-purple-500/10 text-purple-600',
  applied: 'bg-green-500/10 text-green-600',
  rejected: 'bg-red-500/10 text-red-600',
  rolled_back: 'bg-surface-tertiary text-text-muted',
}

/** Rollback exists only for suggestions that changed agent config. */
function canRollback(s: EvolutionSuggestion): boolean {
  if (s.status !== 'applied') return false
  if (s.applied_change) return true
  return s.suggestion_type === 'threshold' && !!s.parameters && '_baseline' in s.parameters
}

function confirmDescription(s: EvolutionSuggestion, action: 'approved' | 'rejected' | 'rolled_back'): string {
  if (action === 'rolled_back') return 'Restores the exact configuration from before the change. Refused if the setting was edited since.'
  if (action === 'rejected') return 'This action will update the suggestion status.'
  if (s.suggestion_type === 'tool_order') return `Adds "${String(s.parameters?.tool ?? '')}" to this agent's tool deny list. Other agents are not affected.`
  if (s.suggestion_type === 'skill_add') return "Creates a new private skill from the suggestion's draft."
  return 'Advisory only: marks the suggestion as reviewed. No agent configuration is changed.'
}

export function EvolutionSuggestionsList({ suggestions, loading, onUpdateStatus }: EvolutionSuggestionsListProps) {
  const { t } = useTranslation('agents')
  const [confirm, setConfirm] = useState<{ s: EvolutionSuggestion; action: 'approved' | 'rejected' | 'rolled_back' } | null>(null)

  if (loading) return <div className="h-24 animate-pulse rounded-lg bg-surface-tertiary" />
  if (suggestions.length === 0) return <p className="text-xs text-text-muted text-center py-6">{t('detail.evolutionTab.noSuggestions')}</p>

  return (
    <div className="space-y-3">
      <h4 className="text-xs font-semibold text-text-primary">{t('detail.evolutionTab.suggestions')}</h4>
      <div className="space-y-2 max-h-80 overflow-y-auto">
        {suggestions.map((s) => (
          <div key={s.id} className="rounded-lg border border-border p-3 space-y-2">
            <div className="flex items-center justify-between">
              <div className="flex items-center gap-1.5">
                <span className={`text-[9px] px-1.5 py-0.5 rounded font-medium ${TYPE_COLORS[s.suggestion_type] ?? 'bg-surface-tertiary text-text-muted'}`}>
                  {s.suggestion_type}
                </span>
                <span className={`text-[9px] px-1.5 py-0.5 rounded ${STATUS_COLORS[s.status] ?? 'bg-surface-tertiary text-text-muted'}`}>
                  {s.status}
                </span>
              </div>
              <span className="text-[10px] text-text-muted">{formatRelativeTime(s.created_at)}</span>
            </div>
            <p className="text-xs text-text-primary line-clamp-2">{s.suggestion}</p>
            {s.rationale && <p className="text-[11px] text-text-muted line-clamp-1">{s.rationale}</p>}
            <div className="flex justify-end gap-1.5">
              {s.status === 'pending' && (
                <>
                  <button onClick={() => setConfirm({ s, action: 'approved' })} className="px-2 py-1 text-[10px] rounded bg-success/10 text-success hover:bg-success/20 transition-colors">{t('detail.evolutionTab.approve')}</button>
                  <button onClick={() => setConfirm({ s, action: 'rejected' })} className="px-2 py-1 text-[10px] rounded bg-error/10 text-error hover:bg-error/20 transition-colors">{t('detail.evolutionTab.reject')}</button>
                </>
              )}
              {canRollback(s) && (
                <button onClick={() => setConfirm({ s, action: 'rolled_back' })} className="px-2 py-1 text-[10px] rounded bg-orange-500/10 text-orange-600 hover:bg-orange-500/20 transition-colors">{t('detail.evolutionTab.rollback')}</button>
              )}
            </div>
          </div>
        ))}
      </div>

      <ConfirmDialog
        open={confirm !== null}
        onOpenChange={() => setConfirm(null)}
        title={confirm ? `${confirm.action === 'approved' ? 'Approve' : confirm.action === 'rejected' ? 'Reject' : 'Rollback'} Suggestion` : ''}
        description={confirm ? confirmDescription(confirm.s, confirm.action) : ''}
        variant={confirm?.action === 'rejected' ? 'destructive' : 'default'}
        onConfirm={async () => {
          if (confirm) await onUpdateStatus(confirm.s.id, confirm.action)
          setConfirm(null)
        }}
      />
    </div>
  )
}
