import {
  act,
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { CodespacePage } from "../CodespacePage";

const fetchJSON = vi.hoisted(() => vi.fn());
vi.mock("../../api/sourceApiFetch", () => ({ fetchJSON }));
vi.mock("../../i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));
vi.mock("../../layouts", () => ({
  MainContent: ({ children }: { children: ReactNode }) => children,
  useNavigationLayout: () => ({ isWideScreen: true }),
}));
vi.mock("../../components/PageHeader", () => ({
  PageHeader: ({ actions }: { actions: ReactNode }) => actions,
}));
vi.mock("../../hooks/useTooltipAppearance", () => ({
  useTextTooltipAttributes: () => ({}),
}));
vi.mock("../../components/ui/ConfirmDialog", () => ({
  confirmDialog: async () => true,
}));

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.resetAllMocks();
});

describe("Codespace management page", () => {
  it("hides USB when unsupported, shows the current ports and sends edits through CLI routes", async () => {
    fetchJSON.mockImplementation(async (path: string) =>
      path.endsWith("capabilities")
        ? { ports: true, usb: false }
        : {
            endpoints: [
              { port: 8080, label: "App", visibility: "authenticated" },
            ],
          },
    );
    render(<CodespacePage />);
    await screen.findByText("8080 · App");
    expect(screen.queryByRole("tab", { name: "USB" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "codespaceUnpublish" }));
    await waitFor(() =>
      expect(fetchJSON).toHaveBeenCalledWith(
        "/codespace/ports/8080",
        expect.objectContaining({ method: "DELETE" }),
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "codespacePublish" }));
    fireEvent.change(screen.getByLabelText("codespacePort"), {
      target: { value: "3000" },
    });
    fireEvent.change(screen.getByLabelText("codespaceLabel"), {
      target: { value: "界".repeat(43) },
    });
    fireEvent.submit(document.getElementById("codespace-port")!);
    expect(await screen.findByText("codespaceLabelLength")).toBeTruthy();
    expect(
      fetchJSON.mock.calls.some(([, options]) => options?.method === "POST"),
    ).toBe(false);
    fireEvent.change(screen.getByLabelText("codespaceLabel"), {
      target: { value: ` ${"界".repeat(42)}ab ` },
    });
    fireEvent.submit(document.getElementById("codespace-port")!);
    await waitFor(() =>
      expect(fetchJSON).toHaveBeenCalledWith(
        "/codespace/ports",
        expect.objectContaining({
          method: "POST",
          body: JSON.stringify({
            port: 3000,
            label: `${"界".repeat(42)}ab`,
            visibility: "authenticated",
          }),
        }),
      ),
    );
  });

  it("merges attached/exported devices, refreshes the visible tab and confirms disconnect identity", async () => {
    const device = {
      source: "workstation",
      busId: "1-3",
      description: "USB disk",
    };
    fetchJSON.mockImplementation(async (path: string) => {
      if (path.endsWith("capabilities")) return { ports: true, usb: true };
      if (path.endsWith("/ports")) return { endpoints: [] };
      return {
        version: 1,
        sources: [{ name: "workstation" }],
        devices: [
          {
            ...device,
            ...(path.endsWith("status")
              ? { port: 0, state: "Port in Use" }
              : {}),
          },
          ...(path.endsWith("status")
            ? [1, 2].map((port) => ({
                source: "",
                busId: "",
                port,
                state: "Port in Use",
                description: `Untracked device ${port}`,
              }))
            : []),
        ],
      };
    });
    render(<CodespacePage />);
    fireEvent.click(await screen.findByRole("tab", { name: "USB" }));
    await screen.findByText("USB disk");
    expect(screen.getAllByText("USB disk")).toHaveLength(1);
    expect(screen.getByText("Untracked device 1")).toBeTruthy();
    expect(screen.getByText("Untracked device 2")).toBeTruthy();
    expect(
      screen.queryByRole("button", { name: "codespaceConnect" }),
    ).toBeNull();
    fireEvent.click(
      screen.getAllByRole("button", { name: "codespaceDisconnect" })[0]!,
    );
    await waitFor(() =>
      expect(fetchJSON).toHaveBeenCalledWith(
        "/codespace/usb/detach",
        expect.objectContaining({
          body: JSON.stringify({
            port: 0,
            source: "workstation",
            busId: "1-3",
          }),
        }),
      ),
    );
    await waitFor(() =>
      expect(
        screen.getAllByRole("button", { name: "codespaceDisconnect" })[0]!,
      ).not.toHaveProperty("disabled", true),
    );
    vi.useFakeTimers();
    fireEvent.click(screen.getByRole("button", { name: "codespaceRefresh" }));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    const before = fetchJSON.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_000);
    });
    expect(fetchJSON.mock.calls.length).toBeGreaterThan(before);
    const visibility = vi.spyOn(document, "hidden", "get");
    visibility.mockReturnValue(true);
    fireEvent(document, new Event("visibilitychange"));
    const hidden = fetchJSON.mock.calls.length;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(fetchJSON.mock.calls).toHaveLength(hidden);
    visibility.mockReturnValue(false);
    await act(async () => {
      fireEvent(document, new Event("visibilitychange"));
    });
    expect(fetchJSON.mock.calls.length).toBeGreaterThan(hidden);
  });
});
