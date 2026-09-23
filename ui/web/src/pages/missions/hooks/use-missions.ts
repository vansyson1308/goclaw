import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";
import { useHttp } from "@/hooks/use-ws";
import { ACTIVE_MISSION_STATUSES, type Mission, type MissionEvent } from "@/types/mission";

const keys = {
  list: ["missions", "list"] as const,
  detail: (id: string) => ["missions", "detail", id] as const,
  events: (id: string) => ["missions", "events", id] as const,
};

const isActive = (m?: Mission) => !!m && ACTIVE_MISSION_STATUSES.includes(m.status);

/** Mission list; polls while any mission is still active. */
export function useMissions() {
  const http = useHttp();
  const query = useQuery({
    queryKey: keys.list,
    queryFn: () => http.get<{ missions: Mission[]; enabled: boolean }>("/v1/missions", { limit: "100" }),
    refetchInterval: (q) => (q.state.data?.missions.some(isActive) ? 3000 : false),
  });
  return {
    missions: query.data?.missions ?? [],
    enabled: query.data?.enabled ?? true,
    loading: query.isLoading,
    error: query.error,
  };
}

/** One mission + its events; polls every 2s until terminal. */
export function useMission(id: string) {
  const http = useHttp();
  const detail = useQuery({
    queryKey: keys.detail(id),
    queryFn: () => http.get<Mission>(`/v1/missions/${id}`),
    refetchInterval: (q) => (isActive(q.state.data) ? 2000 : false),
  });
  const events = useQuery({
    queryKey: keys.events(id),
    queryFn: () => http.get<MissionEvent[]>(`/v1/missions/${id}/events`),
    refetchInterval: isActive(detail.data) ? 2000 : false,
  });
  return { mission: detail.data, events: events.data ?? [], loading: detail.isLoading, error: detail.error };
}

export function useMissionActions() {
  const http = useHttp();
  const qc = useQueryClient();
  const create = useCallback(async (contract: unknown) => {
    const m = await http.post<Mission>("/v1/missions", contract);
    await qc.invalidateQueries({ queryKey: keys.list });
    return m;
  }, [http, qc]);
  const cancel = useCallback(async (id: string) => {
    await http.post<Mission>(`/v1/missions/${id}/cancel`, {});
    await Promise.all([
      qc.invalidateQueries({ queryKey: keys.detail(id) }),
      qc.invalidateQueries({ queryKey: keys.events(id) }),
      qc.invalidateQueries({ queryKey: keys.list }),
    ]);
  }, [http, qc]);
  return { create, cancel };
}
