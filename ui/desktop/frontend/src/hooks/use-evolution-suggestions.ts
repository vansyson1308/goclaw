import { useState, useEffect, useCallback } from 'react'
import { getApiClient, isApiClientReady } from '../lib/api'
import { toast } from '../stores/toast-store'
import type { EvolutionSuggestion } from '../types/evolution'

export function useEvolutionSuggestions(agentId: string) {
  const [suggestions, setSuggestions] = useState<EvolutionSuggestion[]>([])
  const [loading, setLoading] = useState(true)

  const fetchSuggestions = useCallback(async () => {
    if (!isApiClientReady()) { setLoading(false); return }
    try {
      // The endpoint returns a bare JSON array; the object form is accepted
      // for robustness. Reading only `.suggestions` left the list always empty.
      const res = await getApiClient().getWithParams<EvolutionSuggestion[] | { suggestions: EvolutionSuggestion[] | null } | null>(
        `/v1/agents/${agentId}/evolution/suggestions`,
        { limit: '100' },
      )
      setSuggestions(Array.isArray(res) ? res : (res?.suggestions ?? []))
    } catch (err) {
      console.error('Failed to fetch evolution suggestions:', err)
    } finally {
      setLoading(false)
    }
  }, [agentId])

  useEffect(() => { fetchSuggestions() }, [fetchSuggestions])

  const updateStatus = useCallback(async (suggestionId: string, status: 'approved' | 'rejected' | 'rolled_back') => {
    try {
      const res = await getApiClient().patch<{ suggestion?: EvolutionSuggestion }>(
        `/v1/agents/${agentId}/evolution/suggestions/${suggestionId}`, { status },
      )
      // Use the server's resulting state: approving may yield "applied"
      // (tool_order) or stay "approved" (advisory), not the requested status.
      const updated = res?.suggestion
      if (updated) {
        setSuggestions((prev) => prev.map((s) => s.id === suggestionId ? updated : s))
      } else {
        await fetchSuggestions()
      }
    } catch (err) {
      console.error('Failed to update suggestion:', err)
      toast.error('Failed to update suggestion', (err as Error).message)
      await fetchSuggestions()
    }
  }, [agentId, fetchSuggestions])

  return { suggestions, loading, updateStatus }
}
