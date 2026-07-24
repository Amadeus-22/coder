import { ChevronRightIcon, PlusIcon } from "lucide-react";
import { type FC, useId, useState } from "react";
import { Link, useNavigate } from "react-router";
import type * as TypesGen from "#/api/typesGenerated";
import { ErrorAlert } from "#/components/Alert/ErrorAlert";
import { Avatar } from "#/components/Avatar/Avatar";
import { AvatarData } from "#/components/Avatar/AvatarData";
import { Button } from "#/components/Button/Button";
import {
	Dialog,
	DialogContent,
	DialogDescription,
	DialogFooter,
	DialogHeader,
	DialogTitle,
} from "#/components/Dialog/Dialog";
import { Label } from "#/components/Label/Label";
import {
	SettingsHeader,
	SettingsHeaderDescription,
	SettingsHeaderTitle,
} from "#/components/SettingsHeader/SettingsHeader";
import { Switch } from "#/components/Switch/Switch";
import {
	Table,
	TableBody,
	TableCell,
	TableHead,
	TableHeader,
	TableRow,
} from "#/components/Table/Table";
import { TableLoader } from "#/components/TableLoader/TableLoader";
import { useClickableTableRow } from "#/hooks/useClickableTableRow";

type OAuth2AppsSettingsProps = {
	apps?: TypesGen.OAuth2ProviderApp[];
	isLoading: boolean;
	error: unknown;
	canCreateApp: boolean;
	canEditSettings: boolean;
	dynamicClientRegistrationEnabled: boolean | undefined;
	onDynamicClientRegistrationChange: (enabled: boolean) => void;
};

const OAuth2AppsSettingsPageView: FC<OAuth2AppsSettingsProps> = ({
	apps,
	isLoading,
	error,
	canCreateApp,
	canEditSettings,
	dynamicClientRegistrationEnabled,
	onDynamicClientRegistrationChange,
}) => {
	const dcrSwitchId = useId();
	const [isEnableDcrDialogOpen, setIsEnableDcrDialogOpen] = useState(false);

	return (
		<>
			<div className="flex flex-row gap-4 items-baseline justify-between">
				<div>
					<SettingsHeader>
						<SettingsHeaderTitle>OAuth2 Applications</SettingsHeaderTitle>
						<SettingsHeaderDescription>
							Configure applications to use Coder as an OAuth2 provider.
						</SettingsHeaderDescription>
					</SettingsHeader>
				</div>

				{canCreateApp && (
					<Button variant="outline" asChild>
						<Link to="/deployment/oauth2-provider/apps/add">
							<PlusIcon />
							Add application
						</Link>
					</Button>
				)}
			</div>

			{error && <ErrorAlert error={error} />}

			{dynamicClientRegistrationEnabled !== undefined && (
				<div className="flex flex-row items-center gap-3 mt-6">
					<Switch
						id={dcrSwitchId}
						checked={dynamicClientRegistrationEnabled}
						disabled={!canEditSettings}
						onCheckedChange={(checked) => {
							if (checked) {
								setIsEnableDcrDialogOpen(true);
							} else {
								onDynamicClientRegistrationChange(false);
							}
						}}
					/>
					<Label htmlFor={dcrSwitchId}>Dynamic Client Registration</Label>
				</div>
			)}

			<Dialog
				open={isEnableDcrDialogOpen}
				onOpenChange={setIsEnableDcrDialogOpen}
			>
				<DialogContent className="flex flex-col gap-12 max-w-lg">
					<DialogHeader className="flex flex-col gap-4">
						<DialogTitle>Enable Dynamic Client Registration</DialogTitle>
						<DialogDescription>
							Warning: Any OAuth2 client will be able to register itself against
							this deployment (RFC 7591) without prior approval from an
							administrator. Only enable this if you intend to support
							self-service client registration.
						</DialogDescription>
					</DialogHeader>
					<DialogFooter className="flex flex-row">
						<Button
							variant="outline"
							onClick={() => setIsEnableDcrDialogOpen(false)}
						>
							Cancel
						</Button>
						<Button
							onClick={() => {
								setIsEnableDcrDialogOpen(false);
								onDynamicClientRegistrationChange(true);
							}}
						>
							Confirm
						</Button>
					</DialogFooter>
				</DialogContent>
			</Dialog>

			<Table className="mt-8">
				<TableHeader>
					<TableRow>
						<TableHead>Name</TableHead>
						<TableHead className="w-[1%]" />
					</TableRow>
				</TableHeader>
				<TableBody>
					{isLoading && <TableLoader />}
					{apps?.map((app) => (
						<OAuth2AppRow key={app.id} app={app} />
					))}
					{apps?.length === 0 && (
						<TableRow>
							<TableCell colSpan={999}>
								<div className="text-center">
									No OAuth2 applications have been configured.
								</div>
							</TableCell>
						</TableRow>
					)}
				</TableBody>
			</Table>
		</>
	);
};

type OAuth2AppRowProps = {
	app: TypesGen.OAuth2ProviderApp;
};

const OAuth2AppRow: FC<OAuth2AppRowProps> = ({ app }) => {
	const navigate = useNavigate();
	const clickableProps = useClickableTableRow({
		onClick: () => navigate(`/deployment/oauth2-provider/apps/${app.id}`),
	});

	return (
		<TableRow key={app.id} data-testid={`app-${app.id}`} {...clickableProps}>
			<TableCell>
				<AvatarData
					avatar={<Avatar variant="icon" src={app.icon} fallback={app.name} />}
					title={app.name}
				/>
			</TableCell>

			<TableCell>
				<div className="flex pl-4">
					<ChevronRightIcon className="size-icon-sm" />
				</div>
			</TableCell>
		</TableRow>
	);
};

export default OAuth2AppsSettingsPageView;
