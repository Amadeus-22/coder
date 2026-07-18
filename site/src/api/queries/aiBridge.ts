import type { UseInfiniteQueryOptions } from "react-query";
import { API } from "#/api/api";
import type * as TypesGen from "#/api/typesGenerated";
import { useFilterParamsKey } from "#/components/Filter/Filter";
import type { UsePaginatedQueryOptions } from "#/hooks/usePaginatedQuery";

const SESSION_THREADS_INFINITE_PAGE_SIZE = 20;

const aiBridgeKey = ["aiBridge"] as const;

export const paginatedSessions = (
	searchParams: URLSearchParams,
): UsePaginatedQueryOptions<TypesGen.AIBridgeListSessionsResponse, string> => {
	return {
		searchParams,
		queryPayload: () => searchParams.get(useFilterParamsKey) ?? "",
		queryKey: ({ limit, offset, payload }) => {
			return ["aiBridgeSessions", limit, offset, payload] as const;
		},
		queryFn: ({ limit, offset, payload }) =>
			API.getAIBridgeSessionList({
				offset,
				limit,
				q: payload,
			}),
	};
};

export const infiniteSessionThreads = (sessionId: string) => {
	return {
		queryKey: ["aiBridgeSessionThreads", sessionId],
		getNextPageParam: (lastPage: TypesGen.AIBridgeSessionThreadsResponse) => {
			const threads = lastPage.threads;
			if (threads.length < SESSION_THREADS_INFINITE_PAGE_SIZE) {
				return undefined;
			}
			return threads.at(-1)?.id;
		},
		initialPageParam: undefined as string | undefined,
		queryFn: ({ pageParam }) =>
			API.getAIBridgeSessionThreads(sessionId, {
				limit: SESSION_THREADS_INFINITE_PAGE_SIZE,
				after_id: pageParam as string | undefined,
			}),
	} satisfies UseInfiniteQueryOptions<TypesGen.AIBridgeSessionThreadsResponse>;
};

interface AICostParams {
	start_date?: string;
	end_date?: string;
	client?: string;
}

export const userAICostSummary = (user = "me", params?: AICostParams) => ({
	queryKey: [...aiBridgeKey, "costSummary", user, params] as const,
	queryFn: () => API.getUserAICostSummary(user, params),
	staleTime: 60_000,
});

interface PaginatedAICostUsersPayload extends AICostParams {
	search: string;
	start_date: string;
	end_date: string;
}

export const paginatedAICostUsers = (
	payload: PaginatedAICostUsersPayload,
): UsePaginatedQueryOptions<
	TypesGen.AIBridgeCostUsersResponse,
	PaginatedAICostUsersPayload
> => ({
	queryPayload: () => payload,
	queryKey: ({ payload, pageNumber }) =>
		[...aiBridgeKey, "costUsers", payload, pageNumber] as const,
	queryFn: ({ payload, limit, offset }) =>
		API.getAIBridgeCostUsers({
			start_date: payload.start_date,
			end_date: payload.end_date,
			client: payload.client,
			search: payload.search || undefined,
			limit,
			offset,
		}),
	staleTime: 60_000,
});
