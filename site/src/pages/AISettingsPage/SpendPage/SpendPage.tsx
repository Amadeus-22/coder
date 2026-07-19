import dayjs from "dayjs";
import { type FC, useState } from "react";
import { useQuery } from "react-query";
import { useSearchParams } from "react-router";
import {
	paginatedAICostUsers,
	userAICostSummary,
} from "#/api/queries/aiBridge";
import { user } from "#/api/queries/users";
import type { AIBridgeCostUserRollup } from "#/api/typesGenerated";
import type { DateRangeValue } from "#/components/DateRangePicker/DateRangePicker";
import { useDebouncedValue } from "#/hooks/debounce";
import { useAuthenticated } from "#/hooks/useAuthenticated";
import { usePaginatedQuery } from "#/hooks/usePaginatedQuery";
import { useDashboard } from "#/modules/dashboard/useDashboard";
import { RequirePermission } from "#/modules/permissions/RequirePermission";
import { SpendPageView } from "./SpendPageView";
import { toExclusiveEndOfDayDateRange } from "./utils/dateRange";

const startDateSearchParam = "startDate";
const endDateSearchParam = "endDate";
const DEFAULT_DATE_RANGE_DAYS = 30;
const SEARCH_DEBOUNCE_MS = 300;
const USAGE_USERS_PAGE_SIZE = 10;

const getDefaultDateRange = (now?: dayjs.Dayjs): DateRangeValue => {
	const end = now ?? dayjs();
	return {
		startDate: end.subtract(DEFAULT_DATE_RANGE_DAYS, "day").toDate(),
		endDate: end.toDate(),
	};
};

interface SpendPageProps {
	now?: dayjs.Dayjs;
}

const SpendPage: FC<SpendPageProps> = ({ now }) => {
	const { permissions } = useAuthenticated();
	const [searchParams, setSearchParams] = useSearchParams();

	const searchFilter = searchParams.get("search") ?? "";
	const debouncedSearch = useDebouncedValue(searchFilter, SEARCH_DEBOUNCE_MS);
	const setSearchFilter = (value: string) => {
		setSearchParams(
			(prev) => {
				const next = new URLSearchParams(prev);
				if (value) {
					next.set("search", value);
				} else {
					next.delete("search");
				}
				next.delete("page");
				return next;
			},
			{ replace: true },
		);
	};

	const startDateParam = searchParams.get(startDateSearchParam)?.trim() ?? "";
	const endDateParam = searchParams.get(endDateSearchParam)?.trim() ?? "";
	const [defaultDateRange] = useState(() => getDefaultDateRange(now));
	let dateRange = defaultDateRange;
	let endDateIsExclusive = false;

	if (startDateParam && endDateParam) {
		const parsedStartDate = new Date(startDateParam);
		const parsedEndDate = new Date(endDateParam);
		if (
			!Number.isNaN(parsedStartDate.getTime()) &&
			!Number.isNaN(parsedEndDate.getTime()) &&
			parsedStartDate.getTime() <= parsedEndDate.getTime()
		) {
			dateRange = {
				startDate: parsedStartDate,
				endDate: parsedEndDate,
			};
			endDateIsExclusive = true;
		}
	}

	const dateRangeParams = {
		start_date: dateRange.startDate.toISOString(),
		end_date: dateRange.endDate.toISOString(),
	};
	const onDateRangeChange = (value: DateRangeValue) => {
		const nextDateRange = toExclusiveEndOfDayDateRange(value);
		setSearchParams(
			(prev) => {
				const next = new URLSearchParams(prev);
				next.set(startDateSearchParam, nextDateRange.startDate.toISOString());
				next.set(endDateSearchParam, nextDateRange.endDate.toISOString());
				next.delete("page");
				return next;
			},
			{ replace: true },
		);
	};

	const { entitlements, experiments } = useDashboard();
	// TODO(AIGOV-443): drop the experiment gate once cost control is stable.
	const isEntitled =
		(entitlements.features.aibridge.entitlement === "entitled" ||
			entitlements.features.aibridge.entitlement === "grace_period") &&
		entitlements.features.aibridge.enabled &&
		experiments.includes("ai-gateway-cost-control");

	const usersQuery = usePaginatedQuery({
		...paginatedAICostUsers({
			...dateRangeParams,
			search: debouncedSearch,
		}),
		recordsPerPage: USAGE_USERS_PAGE_SIZE,
		preventScrollReset: true,
		enabled: isEntitled,
	});

	const selectedUserId = searchParams.get("user") || null;
	const selectedUserQuery = useQuery({
		...user(selectedUserId ?? ""),
		enabled: selectedUserId !== null,
	});
	const summaryQuery = useQuery({
		...userAICostSummary(selectedUserId ?? "me", dateRangeParams),
		enabled: selectedUserId !== null && isEntitled,
	});

	return (
		<RequirePermission
			isFeatureVisible={permissions.viewAnyAIBridgeInterception}
		>
			<SpendPageView
				dateRange={dateRange}
				endDateIsExclusive={endDateIsExclusive}
				onDateRangeChange={onDateRangeChange}
				searchFilter={searchFilter}
				onSearchFilterChange={setSearchFilter}
				isUsageEntitled={isEntitled}
				usersQuery={usersQuery}
				drillInUserId={selectedUserId}
				drillInUser={selectedUserQuery.data ?? null}
				isDrillInUserLoading={selectedUserQuery.isLoading}
				isDrillInUserError={selectedUserQuery.isError}
				drillInUserError={selectedUserQuery.error}
				onDrillInUserRetry={() => void selectedUserQuery.refetch()}
				onClearSelectedUser={() => {
					setSearchParams((prev) => {
						const next = new URLSearchParams(prev);
						next.delete("user");
						return next;
					});
				}}
				onSelectUser={
					// Drill-in fetches the user profile and cost summary, both of
					// which require user-read authorization.
					permissions.viewAllUsers
						? (selectedUser: AIBridgeCostUserRollup) => {
								setSearchParams((prev) => {
									const next = new URLSearchParams(prev);
									next.set("user", selectedUser.user_id);
									return next;
								});
							}
						: undefined
				}
				summaryData={summaryQuery.data}
				isSummaryLoading={summaryQuery.isLoading}
				summaryError={summaryQuery.error}
				onSummaryRetry={() => void summaryQuery.refetch()}
			/>
		</RequirePermission>
	);
};

export default SpendPage;
