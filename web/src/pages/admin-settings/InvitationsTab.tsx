import { useRef, useState } from "react";
import { useAuth } from "@/hooks/useAuth";
import {
  captureInvitationAuthority,
  invitationScope,
  type AdminInvitation as Invitation,
  type InvitationDelivery,
  type InvitationAuthority,
  type InvitationDeliveryChoice,
} from "@/api/v2/invitations";
import {
  useAdminInvitations,
  useInvitationCapabilities,
  useResendInvitation,
  useRevokeInvitation,
} from "@/hooks/queries/admin/invitations";
import { useViewerIsOwner } from "@/hooks/queries/admin/users";
import { useAdminServerSettings } from "@/hooks/queries/admin/settings";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

import { AlertTriangle, MailPlus } from "lucide-react";
import { toast } from "sonner";
import { copyTextToClipboard } from "@/lib/clipboard";
import { cn } from "@/lib/utils";
import { useT } from "@/i18n";

import { CreateInvitationForm } from "./CreateInvitationForm";
import { ClaimLinkBox } from "./InvitationDisplay";
import { InvitationList } from "./InvitationList";
import { deliveryMessage, isLinkInvitation, useMounted } from "./invitationRules";
import {
  FULL_SCREEN_DIALOG_CLASS,
  TOUCH_DIALOG_CLOSE_CLASS,
  TOUCH_FIELD_CLASS,
  TOUCH_PRIMARY_ACTION_CLASS,
} from "./touchClasses";
import { FullScreenDialogHeader, SettingsLink } from "./TouchLayoutParts";

export default function InvitationsTab() {
  useAuth();
  return <InvitationManager key={invitationScope()} />;
}
function InvitationManager() {
  const t = useT();
  const viewerId = useAuth().user?.id;
  const viewerIsOwner = useViewerIsOwner(viewerId);
  const capabilities = useInvitationCapabilities();
  const available = capabilities.data?.state === "available";
  const history = useAdminInvitations(available);
  // Invitation links are built on the public URL, and the server refuses to
  // create one without it. Settings that are loading or unreadable block nothing.
  const serverSettings = useAdminServerSettings().data;
  const publicURLMissing =
    serverSettings !== undefined && (serverSettings["server.public_url"] ?? "").trim() === "";
  const invitations = history.data?.pages.flatMap((page) => page.items) ?? [];
  const resend = useResendInvitation();
  const revoke = useRevokeInvitation();
  const mounted = useMounted();
  const busy = useRef(false);
  const createBusy = useRef(false);
  const [createOpen, setCreateOpen] = useState(false);
  const emailDelivery = capabilities.data?.email_delivery === true;
  const [confirmRevoke, setConfirmRevoke] = useState<{
    row: Invitation;
    profileContext: InvitationAuthority;
  } | null>(null);
  const [revokeError, setRevokeError] = useState("");
  const [resendOpen, setResendOpen] = useState(false);
  const [resendResult, setResendResult] = useState<InvitationDelivery | null>(null);
  const [resendError, setResendError] = useState("");
  const [copyError, setCopyError] = useState("");
  async function handleCopy(text: string) {
    try {
      await copyTextToClipboard(text);
      if (mounted.current) {
        setCopyError("");
        toast.success(t("web.admin.settings.invitations.copied"));
      }
    } catch {
      if (mounted.current) setCopyError(t("web.admin.settings.invitations.copyFailed"));
    }
  }
  async function handleResend(id: string, delivery: InvitationDeliveryChoice) {
    if (busy.current) return;
    busy.current = true;
    setResendOpen(true);
    setResendError("");
    setResendResult(null);
    setCopyError("");
    try {
      const result = await resend.mutateAsync({
        id,
        delivery,
        profileContext: captureInvitationAuthority(),
      });
      if (mounted.current) setResendResult(result);
    } catch (error) {
      if (mounted.current)
        setResendError(
          error instanceof Error ? error.message : t("web.admin.settings.invitations.resendFailed"),
        );
    } finally {
      resend.reset();
      busy.current = false;
    }
  }
  async function handleRevoke() {
    if (busy.current || !confirmRevoke) return;
    busy.current = true;
    setRevokeError("");
    try {
      await revoke.mutateAsync({
        id: confirmRevoke.row.id,
        profileContext: confirmRevoke.profileContext,
      });
      if (mounted.current) setConfirmRevoke(null);
    } catch (error) {
      if (mounted.current)
        setRevokeError(
          error instanceof Error ? error.message : t("web.admin.settings.invitations.revokeFailed"),
        );
    } finally {
      revoke.reset();
      busy.current = false;
    }
  }
  if (capabilities.isPending)
    return <p>{t("web.admin.settings.invitations.loadingCapabilities")}</p>;
  if (capabilities.isError)
    return (
      <div role="alert">
        {t("web.admin.settings.invitations.capabilitiesFailed", {
          button: (chunks) => (
            <Button className={TOUCH_FIELD_CLASS} onClick={() => void capabilities.refetch()}>
              {chunks}
            </Button>
          ),
        })}
      </div>
    );
  if (!available) return <p>{t("web.admin.settings.invitations.notConfigured")}</p>;
  return (
    <div className="space-y-6">
      <Dialog
        open={confirmRevoke !== null}
        onOpenChange={(open) => {
          if (!open && !busy.current) setConfirmRevoke(null);
        }}
      >
        <DialogContent className={TOUCH_DIALOG_CLOSE_CLASS}>
          <DialogHeader>
            <DialogTitle>{t("web.admin.settings.invitations.revoke.title")}</DialogTitle>
            <DialogDescription>
              {confirmRevoke && isLinkInvitation(confirmRevoke.row)
                ? t("web.admin.settings.invitations.revoke.linkDescription")
                : t("web.admin.settings.invitations.revoke.emailDescription", {
                    email: String(confirmRevoke?.row.email),
                  })}
            </DialogDescription>
          </DialogHeader>
          {revokeError && <p role="alert">{revokeError}</p>}
          <Button
            variant="outline"
            className={TOUCH_FIELD_CLASS}
            disabled={revoke.isPending}
            onClick={() => {
              if (!busy.current) setConfirmRevoke(null);
            }}
          >
            {t("common.cancel")}
          </Button>
          <Button
            variant="destructive"
            className={TOUCH_FIELD_CLASS}
            disabled={revoke.isPending}
            onClick={() => void handleRevoke()}
          >
            {t("web.admin.settings.invitations.revoke.confirm")}
          </Button>
        </DialogContent>
      </Dialog>
      <Dialog
        open={resendOpen}
        onOpenChange={(open) => {
          if (!open && !busy.current) {
            setResendOpen(false);
            setResendResult(null);
            setCopyError("");
          }
        }}
      >
        <DialogContent className={TOUCH_DIALOG_CLOSE_CLASS}>
          <DialogHeader>
            <DialogTitle>{t("web.admin.settings.invitations.resend.title")}</DialogTitle>
            <DialogDescription className="break-words">
              {resendResult
                ? deliveryMessage(resendResult, t)
                : t("web.admin.settings.invitations.resend.description")}
            </DialogDescription>
          </DialogHeader>
          {resend.isPending && <p>{t("web.admin.settings.invitations.resend.creating")}</p>}
          {resendError && (
            <div role="alert">
              <p>{resendError}</p>
              <p>{t("web.admin.settings.invitations.resend.uncertain")}</p>
              <Button
                className={TOUCH_FIELD_CLASS}
                onClick={async () => {
                  try {
                    await history.restart();
                  } catch {
                    return;
                  }
                  if (mounted.current) {
                    setResendOpen(false);
                    setResendError("");
                  }
                }}
              >
                {t("web.admin.settings.invitations.reloadHistory")}
              </Button>
            </div>
          )}
          {copyError && <p role="alert">{copyError}</p>}
          {resendResult && (
            <ClaimLinkBox
              claimUrl={resendResult.claim_url}
              finePrint={t("web.admin.settings.invitations.resend.finePrint")}
              onCopy={handleCopy}
              onDone={() => {
                setResendOpen(false);
                setResendResult(null);
                setCopyError("");
              }}
            />
          )}
        </DialogContent>
      </Dialog>
      <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
        <p className="text-muted-foreground max-w-xl text-sm">
          {t("web.admin.settings.invitations.intro")}
        </p>
        <Dialog
          open={createOpen}
          onOpenChange={(open) => {
            if (!createBusy.current) {
              setCreateOpen(open);
              setCopyError("");
            }
          }}
        >
          <DialogTrigger asChild>
            <Button size="sm" className={TOUCH_PRIMARY_ACTION_CLASS} disabled={publicURLMissing}>
              <MailPlus className="me-1 h-4 w-4" />
              {t("web.admin.settings.invitations.invite.button")}
            </Button>
          </DialogTrigger>
          {/* Below sm the invite form fills the screen; from sm up it is a centered box. */}
          <DialogContent
            className={cn(
              "max-h-[85vh] overflow-y-auto sm:max-w-lg",
              FULL_SCREEN_DIALOG_CLASS,
              TOUCH_DIALOG_CLOSE_CLASS,
            )}
          >
            <FullScreenDialogHeader
              title={t("web.admin.settings.invitations.invite.title")}
              description={t("web.admin.settings.invitations.invite.description")}
            />
            {copyError && <p role="alert">{copyError}</p>}
            <CreateInvitationForm
              defaultProfile={capabilities.data?.default_profile === true}
              emailDelivery={emailDelivery}
              onBusy={(value) => {
                createBusy.current = value;
              }}
              onReload={() => history.restart()}
              onClose={() => {
                if (!createBusy.current) {
                  setCreateOpen(false);
                  setCopyError("");
                }
              }}
              onCopy={handleCopy}
            />
          </DialogContent>
        </Dialog>
      </div>
      {publicURLMissing && (
        <div className="flex items-start gap-3 rounded-xl border border-amber-500/20 bg-amber-500/5 p-3">
          <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" aria-hidden="true" />
          <p className="text-[13px] leading-relaxed">
            {t("web.admin.settings.invitations.publicUrlMissing", {
              link: (chunks) => <SettingsLink to="/admin/settings/general">{chunks}</SettingsLink>,
            })}
          </p>
        </div>
      )}
      {resendError && !resendOpen && (
        <div role="alert">
          <p>{resendError}</p>
          <Button
            className={TOUCH_FIELD_CLASS}
            onClick={async () => {
              try {
                await history.restart();
                if (mounted.current) setResendError("");
              } catch {
                /* The history query displays the failure. */
              }
            }}
          >
            {t("web.admin.settings.invitations.reloadHistory")}
          </Button>
        </div>
      )}
      {history.isError && (
        <div role="alert">
          <p className="break-words">
            {t("web.admin.settings.invitations.historyFailed", { error: history.error.message })}
          </p>
          <Button
            className={TOUCH_FIELD_CLASS}
            onClick={() => void history.restart().catch(() => {})}
          >
            {t("web.admin.settings.invitations.reloadHistory")}
          </Button>
        </div>
      )}
      {history.isPending ? (
        <p>{t("web.admin.settings.invitations.loading")}</p>
      ) : invitations.length === 0 && !history.isError ? (
        <p>{t("web.admin.settings.invitations.empty")}</p>
      ) : (
        invitations.length > 0 && (
          <InvitationList
            invitations={invitations}
            emailDelivery={emailDelivery}
            viewerIsOwner={viewerIsOwner}
            resending={resend.isPending || revoke.isPending || !!resendError}
            resendBlocked={publicURLMissing}
            onResend={(inv, delivery) => void handleResend(inv.id, delivery)}
            onRevoke={(inv) => {
              if (busy.current) return;
              setRevokeError("");
              setConfirmRevoke({ row: inv, profileContext: captureInvitationAuthority() });
            }}
          />
        )
      )}
      {history.hasNextPage && (
        <Button
          variant="outline"
          className={TOUCH_PRIMARY_ACTION_CLASS}
          disabled={history.isFetchingNextPage || history.isError}
          onClick={() => void history.fetchNextPage()}
        >
          {t("web.admin.settings.invitations.loadMore")}
        </Button>
      )}
    </div>
  );
}
<<<<<<< HEAD

function InvitationRow({
  invitation,
  onResend,
  emailDelivery,
  onRevoke,
  resending,
  resendBlocked,
  resendAllowed,
}: {
  invitation: Invitation;
  onResend: (delivery: InvitationDeliveryChoice) => void;
  emailDelivery: boolean;
  onRevoke: () => void;
  resending: boolean;
  resendBlocked: boolean;
  resendAllowed: boolean;
}) {
  const badge = STATUS_BADGES[invitation.status];
  const showResend =
    resendAllowed && (invitation.status === "pending" || invitation.status === "expired");
  const showRevoke = invitation.status === "pending";
  const link = isLinkInvitation(invitation);
  // A link invitation's note is how the admin told it apart, so it stays
  // after acceptance fills in the address.
  const shared = invitation.delivery === "link";
  const details = [
    !link && invitation.status === "accepted" && shared ? "Joined by link" : "",
    shared && invitation.note ? `“${invitation.note}”` : "",
    invitation.invited_by_name ? `Invited by ${invitation.invited_by_name}` : "",
  ].filter(Boolean);

  return (
    <TableRow>
      <TableCell className="max-w-xs">
        <div className="truncate font-medium">{link ? "Link invitation" : invitation.email}</div>
        {details.length > 0 && (
          <div className="text-muted-foreground truncate text-xs">{details.join(" · ")}</div>
        )}
      </TableCell>
      <TableCell className="text-muted-foreground capitalize">{invitation.role}</TableCell>
      <TableCell>
        <Badge variant={badge.variant}>{badge.label}</Badge>
        {invitation.status === "pending" && (
          <span className="text-muted-foreground ml-2 text-xs">
            expires {formatDate(invitation.expires_at)}
          </span>
        )}
      </TableCell>
      <TableCell>
        <DeliveryLabel delivery={invitation.delivery} />
      </TableCell>
      <TableCell className="text-muted-foreground text-sm">
        {formatDate(invitation.created_at)}
      </TableCell>
      <TableCell>
        <div className="flex justify-end gap-1">
          {showResend && (
            // Replacing the link never emails; an emailed invitation keeps its address.
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onResend("link")}
              disabled={resending || resendBlocked}
              title={link ? "Replace with a new link" : "Copy a new link without emailing"}
            >
              {link ? <RotateCw className="h-4 w-4" /> : <Link2 className="h-4 w-4" />}
            </Button>
          )}
          {showResend && !link && emailDelivery && (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => onResend("email")}
              disabled={resending || resendBlocked}
              title="Email a new link"
            >
              <MailPlus className="h-4 w-4" />
            </Button>
          )}
          {showRevoke && (
            <Button
              variant="ghost"
              size="sm"
              onClick={onRevoke}
              title="Revoke this link"
              disabled={resending}
            >
              <Trash2 className="h-4 w-4" />
            </Button>
          )}
        </div>
      </TableCell>
    </TableRow>
  );
}

function CreateInvitationForm({
  onClose,
  onCopy,
  defaultProfile,
  emailDelivery,
  onBusy,
  onReload,
}: {
  defaultProfile: boolean;
  emailDelivery: boolean;
  onBusy: (value: boolean) => void;
  onReload: () => Promise<void>;
  onClose: () => void;
  onCopy: (text: string) => Promise<void>;
}) {
  const create = useCreateInvitation();
  const { data: accessGroups = [] } = useAccessGroups();
  const { data: libraries = [] } = useAdminLibraries();
  const [delivery, setDelivery] = useState<InvitationDeliveryChoice>(
    emailDelivery ? "email" : "link",
  );
  const deliveryGroup = useId();
  const [email, setEmail] = useState("");
  const [emailInvalid, setEmailInvalid] = useState(false);
  const [role, setRole] = useState<"user" | "admin">("user");
  // Only the server Owner may invite an admin; the server refuses anyone else.
  const viewerId = useAuth().user?.id;
  const viewerIsOwner = useViewerIsOwner(viewerId);
  const [accessGroupID, setAccessGroupID] = useState<number | null>(null);
  const [libraryIDs, setLibraryIDs] = useState<number[] | null>(null);
  const [note, setNote] = useState("");
  const [createProfile, setCreateProfile] = useState(defaultProfile);
  const [showTour, setShowTour] = useState(true);
  // After creation we keep the dialog open to show the claim link — the
  // token is only readable in this response, so this is the one chance to
  // copy it. The delivery outcome describes what the sender confirmed.
  const [result, setResult] = useState<InvitationDelivery | null>(null);
  const [error, setError] = useState("");
  const [needsReload, setNeedsReload] = useState(false);
  const busy = useRef(false);
  const mounted = useMounted();
  const [profileContext] = useState(captureInvitationAuthority);
  const defaultGroup = useMemo(() => accessGroups.find((g) => g.is_default), [accessGroups]);
  async function handleSubmit(e: FormEvent) {
    e.preventDefault();
    if (busy.current || needsReload || (!defaultProfile && createProfile)) return;
    if (delivery === "email" && !isValidEmail(email)) {
      setEmailInvalid(true);
      return;
    }
    busy.current = true;
    onBusy(true);
    setError("");
    const group = effectiveAccessGroupID(role, accessGroupID);
    try {
      const delivered = await create.mutateAsync({
        profileContext,
        body: {
          delivery,
          email: delivery === "email" ? email : undefined,
          role,
          access_group_id: group == null ? undefined : String(group),
          library_ids: libraryIDs == null ? undefined : libraryIDs.map(String),
          create_profile: createProfile,
          show_tour: showTour,
          note: note.trim() || undefined,
        },
      });
      if (mounted.current) setResult(delivered);
    } catch (err) {
      if (mounted.current) {
        setError(err instanceof Error ? err.message : "Unable to create invitation.");
        setNeedsReload(true);
      }
    } finally {
      create.reset();
      busy.current = false;
      onBusy(false);
    }
  }

  if (result) {
    return (
      <div className="min-w-0 space-y-4">
        <p className="text-sm">{deliveryMessage(result)}</p>
        <ClaimLinkBox
          claimUrl={result.claim_url}
          finePrint={
            result.delivery_status === "not_requested"
              ? "The link works once and expires in 7 days. Anyone with the link can use it. Revoke it from the history if it ends up with the wrong person."
              : "The link works once and expires in 7 days. Resending later mints a fresh link and kills this one."
          }
          onCopy={onCopy}
          onDone={onClose}
        />
      </div>
    );
  }

  return (
    <form onSubmit={handleSubmit} className="space-y-4">
      {error && (
        <div role="alert">
          <p>{error}</p>
          <p>Reload history before another deliberate attempt; the invitation may already exist.</p>
          <Button
            type="button"
            onClick={async () => {
              try {
                await onReload();
              } catch {
                if (mounted.current)
                  setError(
                    "Could not reload invitation history. Try again before creating another invitation.",
                  );
                return;
              }
              if (mounted.current) {
                setNeedsReload(false);
                setError("");
              }
            }}
          >
            Reload history
          </Button>
        </div>
      )}
      {!defaultProfile && (
        <p role="status">
          This server cannot create a default profile atomically. Turn off the profile option to
          create a profileless invitation.
        </p>
      )}
      <div className="space-y-2">
        <p id={`${deliveryGroup}-label`} className="text-sm leading-none font-medium">
          How should they get it?
        </p>
        <div
          role="radiogroup"
          aria-labelledby={`${deliveryGroup}-label`}
          className="grid gap-2 sm:grid-cols-2"
        >
          <ChoiceOption
            name={deliveryGroup}
            value="link"
            selected={delivery === "link"}
            disabled={create.isPending}
            title="Create link"
            onSelect={setDelivery}
          >
            You share the link yourself, for example in a chat.
          </ChoiceOption>
          <ChoiceOption
            name={deliveryGroup}
            value="email"
            selected={delivery === "email"}
            disabled={!emailDelivery || create.isPending}
            title="Send email"
            onSelect={setDelivery}
          >
            {emailDelivery
              ? "We email the link. You can copy it too."
              : "Email isn't set up on this server."}
          </ChoiceOption>
        </div>
        {!emailDelivery && (
          <div className="flex items-start gap-3 rounded-xl border border-amber-500/20 bg-amber-500/5 p-3">
            <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0 text-amber-500" aria-hidden="true" />
            <p className="text-[13px] leading-relaxed">
              To send invitations by email, set up the mail server in{" "}
              <Link
                to="/admin/settings/email"
                className="text-foreground inline-flex items-center gap-1 font-medium hover:underline"
              >
                Email settings
                <ArrowRight className="h-3 w-3" aria-hidden="true" />
              </Link>
            </p>
          </div>
        )}
      </div>

      {delivery === "email" && (
        <div className="space-y-2">
          <Label htmlFor="invitation-email">Email address</Label>
          <Input
            id="invitation-email"
            type="email"
            value={email}
            onChange={(e) => {
              setEmail(e.target.value);
              if (emailInvalid) setEmailInvalid(false);
            }}
            placeholder="them@example.com"
            aria-invalid={emailInvalid || undefined}
            aria-describedby={emailInvalid ? "invitation-email-error" : undefined}
            autoFocus
            required
          />
          {emailInvalid ? (
            <p id="invitation-email-error" className="text-destructive text-xs">
              {INVALID_EMAIL_MESSAGE}
            </p>
          ) : null}
          <p className="text-muted-foreground text-xs">
            This becomes both the destination and their sign-in username.
          </p>
        </div>
      )}

      <div className="grid gap-3 sm:grid-cols-2">
        <div className="space-y-2">
          <Label>Access group</Label>
          <Select
            value={accessGroupID === null ? "default" : String(accessGroupID)}
            onValueChange={(v) => setAccessGroupID(v === "default" ? null : Number(v))}
          >
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="default">
                {defaultGroup ? `${defaultGroup.name} (default)` : "Server default"}
              </SelectItem>
              {accessGroups
                .filter((g) => !g.is_default)
                .map((g) => (
                  <SelectItem key={g.id} value={String(g.id)}>
                    {g.name}
                  </SelectItem>
                ))}
            </SelectContent>
          </Select>
        </div>
        <div className="space-y-2">
          <Label>Role</Label>
          <Select value={role} onValueChange={(value) => setRole(value as "user" | "admin")}>
            <SelectTrigger>
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="user">User</SelectItem>
              <SelectItem value="admin" disabled={!viewerIsOwner}>
                Admin
              </SelectItem>
            </SelectContent>
          </Select>
          {!viewerIsOwner && (
            <p className="text-muted-foreground text-xs">
              Only the server owner can invite an admin.
            </p>
          )}
        </div>
      </div>

      <LibraryAccessSelector
        libraries={libraries}
        value={libraryIDs}
        onChange={setLibraryIDs}
        allLabel="Inherit from access group"
        emptyHint="The account created at accept follows the selected group's library scope."
      />

      <div className="space-y-2">
        <Label htmlFor="invitation-note">
          {delivery === "link" ? "Note (optional)" : "Personal note (optional)"}
        </Label>
        <textarea
          id="invitation-note"
          value={note}
          onChange={(e) => setNote(e.target.value)}
          placeholder="Hey — set yourself up whenever."
          rows={2}
          className="border-border bg-background text-foreground focus:border-ring focus:ring-ring/50 w-full resize-y rounded-md border px-3 py-2.5 text-sm shadow-xs transition-[color,box-shadow] outline-none focus:ring-[3px]"
        />
        <p className="text-muted-foreground text-xs">
          {delivery === "link"
            ? "Shown on the invitation page, and in your invitation history so you can tell links apart."
            : "Appears in the email and on the invitation page. Plain text."}
        </p>
      </div>

      <div className="space-y-3">
        <div className="flex items-center justify-between">
          <div>
            <Label htmlFor="invitation-create-profile">Create their first profile</Label>
            <p className="text-muted-foreground text-xs">
              {delivery === "link"
                ? "Named from the part before the @ in the email they enter. They can rename it later."
                : "Named from the part before the @. They can rename it later."}
            </p>
          </div>
          <Switch
            id="invitation-create-profile"
            checked={createProfile}
            onCheckedChange={setCreateProfile}
            disabled={!defaultProfile && !createProfile}
          />
        </div>
        <div className="flex items-center justify-between">
          <div>
            <Label htmlFor="invitation-show-tour">Show the feature tour on first sign-in</Label>
            <p className="text-muted-foreground text-xs">
              Walks through what this server can do, skipping anything turned off.
            </p>
          </div>
          <Switch id="invitation-show-tour" checked={showTour} onCheckedChange={setShowTour} />
        </div>
      </div>

      <div className="flex items-center justify-between pt-2">
        <p className="text-muted-foreground text-xs">Link expires in 7 days · single use</p>
        <div className="flex gap-2">
          <Button type="button" variant="ghost" disabled={create.isPending} onClick={onClose}>
            Cancel
          </Button>
          <Button
            type="submit"
            disabled={create.isPending || needsReload || (!defaultProfile && createProfile)}
          >
            {delivery === "link"
              ? create.isPending
                ? "Creating..."
                : "Create link"
              : create.isPending
                ? "Sending..."
                : "Send invite"}
          </Button>
        </div>
      </div>
    </form>
  );
}
=======
>>>>>>> upstream/main
