import type {
  CodespaceCapabilities,
  CodespaceEndpoint,
  CodespaceUSBDevice,
  CodespaceUSBState,
} from "@yep-anywhere/shared";
import { useEffect, useRef, useState } from "react";
import { fetchJSON } from "../api/sourceApiFetch";
import { PageHeader } from "../components/PageHeader";
import { PencilIcon } from "../components/PencilIcon";
import { SidebarIcons } from "../components/SidebarNavItem";
import { Banner } from "../components/ui/Banner";
import { Button, IconButton } from "../components/ui/Button";
import { confirmDialog } from "../components/ui/ConfirmDialog";
import { Dialog } from "../components/ui/Dialog";
import { Field, Select, Textarea, TextInput } from "../components/ui/Field";
import { TabPanel, Tabs } from "../components/ui/Tabs";
import { useI18n } from "../i18n";
import { MainContent, useNavigationLayout } from "../layouts";
import pageStyles from "../layouts/PageLayout.module.css";
import { writeClipboardText } from "../lib/clipboard";
import styles from "./CodespacePage.module.css";

const emptyUSB: CodespaceUSBState = { version: 1, sources: [], devices: [] };
const sourcePattern = /^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$/;

export function CodespacePage() {
  const { t } = useI18n();
  const { isWideScreen } = useNavigationLayout();
  const [tab, setTab] = useState<"ports" | "usb">("ports");
  const [capabilities, setCapabilities] = useState<CodespaceCapabilities>();
  const [ports, setPorts] = useState<CodespaceEndpoint[]>([]);
  const [usb, setUSB] = useState(emptyUSB);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [loading, setLoading] = useState(true);
  const [busy, setBusy] = useState(false);
  const [revision, setRevision] = useState(0);
  const [editing, setEditing] = useState<CodespaceEndpoint>();
  const [originalPort, setOriginalPort] = useState<number>();
  const [help, setHelp] = useState(false);
  const request = useRef<AbortController | null>(null);
  const mounted = useRef(false);
  const writing = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  // biome-ignore lint/correctness/useExhaustiveDependencies: revision explicitly requests a manual refresh.
  useEffect(() => {
    if (busy) return;
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const refresh = async () => {
      clearTimeout(timer);
      request.current?.abort();
      if (document.hidden || stopped) return;
      const controller = new AbortController();
      request.current = controller;
      setLoading(true);
      try {
        const options = { signal: controller.signal };
        const caps = await fetchJSON<CodespaceCapabilities>(
          "/codespace/capabilities",
          options,
        );
        if (controller.signal.aborted) return;
        setCapabilities(caps);
        if (tab === "usb" && !caps.usb) setTab("ports");
        else if (tab === "usb") {
          const status = await fetchJSON<CodespaceUSBState>(
            "/codespace/usb/status",
            options,
          );
          const exported =
            status.sources.length > 0
              ? await fetchJSON<CodespaceUSBState>(
                  "/codespace/usb/list",
                  options,
                )
              : emptyUSB;
          const attached = new Set(
            status.devices
              .filter((device) => device.source && device.busId)
              .map((device) => `${device.source}/${device.busId}`),
          );
          if (!controller.signal.aborted)
            setUSB({
              ...status,
              sources: exported.sources.length
                ? exported.sources
                : status.sources,
              devices: [
                ...status.devices,
                ...exported.devices.filter(
                  (device) => !attached.has(`${device.source}/${device.busId}`),
                ),
              ],
            });
        } else if (caps.ports) {
          const result = await fetchJSON<{ endpoints: CodespaceEndpoint[] }>(
            "/codespace/ports",
            options,
          );
          if (!controller.signal.aborted) setPorts(result.endpoints);
        }
        if (!controller.signal.aborted) setError("");
      } catch (failure) {
        if (!controller.signal.aborted)
          setError(
            failure instanceof Error ? failure.message : String(failure),
          );
      } finally {
        if (!controller.signal.aborted && !stopped) {
          setLoading(false);
          timer = setTimeout(refresh, 10_000);
        }
      }
    };
    const visibility = () => {
      void refresh();
    };
    document.addEventListener("visibilitychange", visibility);
    void refresh();
    return () => {
      stopped = true;
      clearTimeout(timer);
      request.current?.abort();
      document.removeEventListener("visibilitychange", visibility);
    };
  }, [tab, revision, busy]);

  const mutate = async (path: string, method: string, body?: unknown) => {
    if (writing.current) return;
    writing.current = true;
    request.current?.abort();
    setBusy(true);
    setActionError("");
    try {
      await fetchJSON(`/codespace/${path}`, {
        method,
        headers: { "Content-Type": "application/json" },
        ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      });
      if (mounted.current) setEditing(undefined);
    } catch (failure) {
      if (mounted.current)
        setActionError(
          failure instanceof Error ? failure.message : String(failure),
        );
    } finally {
      writing.current = false;
      if (mounted.current) setBusy(false);
    }
  };
  const detach = async (device: CodespaceUSBDevice) => {
    if (
      await confirmDialog({
        title: t("codespaceDisconnect"),
        message: `${device.source} / ${device.busId}`,
        confirmLabel: t("codespaceDisconnect"),
        tone: "danger",
      })
    ) {
      await mutate("usb/detach", "POST", {
        port: device.port,
        source: device.source,
        busId: device.busId,
      });
    }
  };
  const blocked = busy || loading || Boolean(error);
  const sourceNames = [
    ...new Set([
      ...usb.sources.map((item) => item.name),
      ...usb.devices.map((item) => item.source),
    ]),
  ];
  return (
    <MainContent isWideScreen={isWideScreen}>
      <PageHeader
        title="Codespace"
        actions={
          <>
            <IconButton
              label={t("codespaceRefresh")}
              busy={loading}
              disabled={busy}
              onClick={() => setRevision((value) => value + 1)}
            >
              <span aria-hidden="true">&#8635;</span>
            </IconButton>
            <IconButton
              label={t("codespaceHelp")}
              onClick={() => setHelp(true)}
            >
              <span aria-hidden="true">?</span>
            </IconButton>
          </>
        }
      />
      <main
        className={pageStyles.pageScrollContainer}
        data-page-scroll-container
      >
        <div
          className={`${pageStyles.pageContentInner} ${styles.content}`}
          data-page-content-inner
        >
          <Tabs
            id="codespace"
            value={tab}
            onChange={setTab}
            tabs={[
              { value: "ports", label: t("codespacePorts") },
              ...(capabilities?.usb
                ? [{ value: "usb" as const, label: "USB" }]
                : []),
            ]}
          />
          {(error || (!editing && actionError)) && (
            <Banner tone="error">{error || actionError}</Banner>
          )}
          <TabPanel tabsId="codespace" value={tab}>
            {tab === "ports" ? (
              <>
                <div className={styles.toolbar}>
                  <h2>{t("codespacePorts")}</h2>
                  {capabilities?.ports && (
                    <IconButton
                      label={t("codespacePublish")}
                      disabled={blocked}
                      onClick={() => {
                        setOriginalPort(undefined);
                        setActionError("");
                        setEditing({
                          port: 3000,
                          label: "",
                          visibility: "authenticated",
                        });
                      }}
                    >
                      {SidebarIcons.newSession}
                    </IconButton>
                  )}
                </div>
                {capabilities && !capabilities.ports && (
                  <p>{t("codespaceUnavailable")}</p>
                )}
                {!loading && capabilities?.ports && ports.length === 0 && (
                  <p>{t("codespaceNoPorts")}</p>
                )}
                <div className={styles.list}>
                  {ports.map((item) => (
                    <div className={styles.row} key={item.port}>
                      <div className={styles.detail}>
                        <strong>
                          {item.port} &middot; {item.label}
                        </strong>
                        <span>
                          {t(
                            item.visibility === "public"
                              ? "codespacePublic"
                              : "codespaceAuthenticated",
                          )}
                        </span>
                      </div>
                      <div className={styles.actions}>
                        <IconButton
                          label={t("codespaceEdit")}
                          disabled={blocked}
                          onClick={() => {
                            setOriginalPort(item.port);
                            setActionError("");
                            setEditing({ ...item });
                          }}
                        >
                          <PencilIcon />
                        </IconButton>
                        <Button
                          disabled={blocked}
                          variant="danger-outline"
                          onClick={() =>
                            void mutate(`ports/${item.port}`, "DELETE")
                          }
                        >
                          {t("codespaceUnpublish")}
                        </Button>
                      </div>
                    </div>
                  ))}
                </div>
              </>
            ) : (
              <>
                {!loading && sourceNames.length === 0 && (
                  <p>{t("codespaceNoUSB")}</p>
                )}
                {sourceNames.map((name) => {
                  const sourceError = usb.sources.find(
                    (item) => item.name === name,
                  )?.error;
                  return (
                    <section key={name} className={styles.source}>
                      <h2>{name || t("codespaceUnknownSource")}</h2>
                      {sourceError && (
                        <Banner tone="error">{sourceError}</Banner>
                      )}
                      <div className={styles.list}>
                        {usb.devices
                          .filter((item) => item.source === name)
                          .map((item) => (
                            <div
                              className={styles.row}
                              key={`${item.busId}/${item.port ?? "exported"}`}
                            >
                              <div className={styles.detail}>
                                <strong>{item.description}</strong>
                                <span>
                                  {item.busId}
                                  {item.port === undefined
                                    ? ""
                                    : ` · VHCI ${item.port} · ${item.state}`}
                                </span>
                              </div>
                              <Button
                                disabled={
                                  blocked ||
                                  (item.port === undefined &&
                                    Boolean(sourceError))
                                }
                                variant={
                                  item.port === undefined
                                    ? "secondary"
                                    : "danger-outline"
                                }
                                onClick={() =>
                                  item.port === undefined
                                    ? void mutate("usb/attach", "POST", {
                                        source: item.source,
                                        busId: item.busId,
                                      })
                                    : void detach(item)
                                }
                              >
                                {t(
                                  item.port === undefined
                                    ? "codespaceConnect"
                                    : "codespaceDisconnect",
                                )}
                              </Button>
                            </div>
                          ))}
                      </div>
                    </section>
                  );
                })}
              </>
            )}
          </TabPanel>
        </div>
      </main>
      {editing && (
        <Dialog
          title={t("codespacePublish")}
          onClose={() => {
            if (!busy) setEditing(undefined);
          }}
          footer={
            <Button
              type="submit"
              form="codespace-port"
              variant="primary"
              busy={busy}
            >
              {t("codespaceSave")}
            </Button>
          }
        >
          {actionError && <Banner tone="error">{actionError}</Banner>}
          <form
            id="codespace-port"
            className={styles.form}
            onSubmit={(event) => {
              event.preventDefault();
              void mutate("ports", "POST", editing);
            }}
          >
            <Field label={t("codespacePort")}>
              <TextInput
                type="number"
                min={1}
                max={65535}
                required
                disabled={busy}
                value={editing.port}
                readOnly={originalPort !== undefined}
                onChange={(event) =>
                  setEditing({ ...editing, port: Number(event.target.value) })
                }
              />
            </Field>
            <Field label={t("codespaceLabel")}>
              <TextInput
                required
                maxLength={128}
                disabled={busy}
                value={editing.label}
                onChange={(event) =>
                  setEditing({ ...editing, label: event.target.value })
                }
              />
            </Field>
            <Field label={t("codespaceVisibility")}>
              <Select
                disabled={busy}
                value={editing.visibility}
                onChange={(event) =>
                  setEditing({
                    ...editing,
                    visibility: event.target
                      .value as CodespaceEndpoint["visibility"],
                  })
                }
              >
                <option value="authenticated">
                  {t("codespaceAuthenticated")}
                </option>
                <option value="public">{t("codespacePublic")}</option>
              </Select>
            </Field>
          </form>
        </Dialog>
      )}
      {help && (
        <ConnectionHelp
          capabilities={capabilities}
          onClose={() => setHelp(false)}
        />
      )}
    </MainContent>
  );
}

function ConnectionHelp({
  capabilities,
  onClose,
}: {
  capabilities?: CodespaceCapabilities;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const [source, setSource] = useState("workstation");
  const [mode, setMode] = useState<"ssh" | "usb">("ssh");
  const [copied, setCopied] = useState(false);
  const text = useRef<HTMLTextAreaElement>(null);
  const ssh = capabilities?.ssh;
  const usb = mode === "usb" && capabilities?.usb;
  const valid = !usb || sourcePattern.test(source);
  const target = ssh ? `-p ${ssh.port} -l ${ssh.user} ${ssh.host}` : "";
  const command =
    ssh && valid
      ? usb
        ? `ssh -NT -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 -o ServerAliveCountMax=3 -R usbip.${source}:3240:127.0.0.1:3240 ${target}`
        : `ssh ${target}`
      : "";
  return (
    <Dialog
      title={t("codespaceHelp")}
      onClose={onClose}
      footer={
        <Button
          disabled={!command}
          onClick={async () => {
            if (await writeClipboardText(command)) setCopied(true);
            else {
              text.current?.focus();
              text.current?.select();
            }
          }}
        >
          {t(copied ? "codespaceCopied" : "codespaceCopy")}
        </Button>
      }
    >
      {capabilities?.usb && (
        <Tabs
          value={mode}
          onChange={(value) => {
            setMode(value);
            setCopied(false);
          }}
          tabs={[
            { value: "ssh", label: "SSH" },
            { value: "usb", label: "USB/IP" },
          ]}
        />
      )}
      {!ssh && <p>{t("codespaceNoSSH")}</p>}
      {usb && (
        <>
          <p>{t("codespaceUSBHelp")}</p>
          <Field
            label={t("codespaceSource")}
            error={valid ? undefined : t("codespaceInvalidSource")}
          >
            <TextInput
              value={source}
              maxLength={32}
              onChange={(event) => {
                setSource(event.target.value);
                setCopied(false);
              }}
            />
          </Field>
        </>
      )}
      {command && (
        <Field label={usb ? "USB/IP SSH" : "SSH"}>
          <Textarea ref={text} value={command} readOnly mono rows={5} />
        </Field>
      )}
    </Dialog>
  );
}
