import type { Meta, StoryObj } from "@storybook/react-vite";
import { expect, fn, within } from "storybook/test";
import type * as TypesGen from "#/api/typesGenerated";
import { AICostSummaryView } from "./AICostSummaryView";

const populatedSummary: TypesGen.AIBridgeUserCostSummary = {
	start_date: "2026-02-10T00:00:00Z",
	end_date: "2026-03-12T00:00:00Z",
	total_cost_micros: 1_500_000,
	request_count: 13,
	priced_request_count: 12,
	unpriced_request_count: 1,
	total_input_tokens: 123_456,
	total_output_tokens: 654_321,
	total_cache_read_tokens: 9_876,
	total_cache_write_tokens: 5_432,
	by_model: [
		{
			provider: "openai",
			model: "gpt-4.1",
			total_cost_micros: 1_250_000,
			request_count: 9,
			unpriced_request_count: 1,
			total_input_tokens: 100_000,
			total_output_tokens: 200_000,
			total_cache_read_tokens: 7_654,
			total_cache_write_tokens: 3_210,
		},
	],
	by_chat: [
		{
			chat_id: "chat-1",
			chat_title: "Quarterly review",
			total_cost_micros: 750_000,
			request_count: 5,
			unpriced_request_count: 0,
			total_input_tokens: 60_000,
			total_output_tokens: 80_000,
			total_cache_read_tokens: 4_321,
			total_cache_write_tokens: 1_234,
		},
		{
			chat_id: "chat-without-title",
			chat_title: "",
			total_cost_micros: 250_000,
			request_count: 2,
			unpriced_request_count: 1,
			total_input_tokens: 20_000,
			total_output_tokens: 30_000,
			total_cache_read_tokens: 1_000,
			total_cache_write_tokens: 500,
		},
	],
};

const emptySummary: TypesGen.AIBridgeUserCostSummary = {
	start_date: "2026-02-10T00:00:00Z",
	end_date: "2026-03-12T00:00:00Z",
	total_cost_micros: 0,
	request_count: 0,
	priced_request_count: 0,
	unpriced_request_count: 0,
	total_input_tokens: 0,
	total_output_tokens: 0,
	total_cache_read_tokens: 0,
	total_cache_write_tokens: 0,
	by_model: [],
	by_chat: [],
};

const meta: Meta<typeof AICostSummaryView> = {
	title: "pages/AISettingsPage/SpendPage/components/AICostSummaryView",
	component: AICostSummaryView,
	args: {
		summary: undefined,
		isLoading: false,
		error: undefined,
		onRetry: fn(),
		loadingLabel: "Loading usage details",
		emptyMessage: "No usage details available.",
	},
};

export default meta;
type Story = StoryObj<typeof AICostSummaryView>;

export const Populated: Story = {
	args: {
		summary: populatedSummary,
	},
	play: async ({ canvasElement }) => {
		const canvas = within(canvasElement);
		await expect(canvas.getAllByText("Requests")).toHaveLength(3);
		await expect(canvas.getByText("Quarterly review")).toBeInTheDocument();
		await expect(canvas.getByText("chat-without-title")).toBeInTheDocument();
		await expect(
			canvas.getByText(/1 request lacked pricing data/),
		).toBeInTheDocument();
	},
};

export const Empty: Story = {
	args: {
		summary: emptySummary,
	},
};

export const Loading: Story = {
	args: {
		isLoading: true,
	},
};

const ErrorState: Story = {
	args: {
		error: new Error("Failed to fetch"),
	},
};

export { ErrorState as Error };
