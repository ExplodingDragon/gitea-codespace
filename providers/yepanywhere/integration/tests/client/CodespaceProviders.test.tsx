// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { CODESPACE_PROVIDERS } from "@yep-anywhere/shared";
import { afterEach, expect, it, vi } from "vitest";
import { getAllProviders } from "../../providers/registry";
import { ProvidersSettings } from "../settings/ProvidersSettings";

vi.mock("../../hooks/useProviders", () => ({
  useProviders: () => ({
    providers: [
      {
        name: "opencode",
        displayName: "OpenCode",
        installed: true,
        authenticated: true,
        enabled: true,
        models: [],
      },
    ],
    loading: false,
    error: null,
    reload: vi.fn(),
    refetch: vi.fn(),
  }),
}));
vi.mock("../../hooks/useServerSettings", () => ({
  useServerSettings: () => ({
    settings: {},
    isLoading: false,
    updateSetting: vi.fn(),
    updateSettings: vi.fn(),
  }),
}));
vi.mock("../../hooks/useVersion", () => ({
  useVersion: () => ({ version: { capabilities: [] } }),
}));
vi.mock("../../contexts/ToastContext", () => ({
  useToastContext: () => ({ showToast: vi.fn() }),
}));
vi.mock("../../i18n", () => ({ useI18n: () => ({ t: (key: string) => key }) }));

afterEach(cleanup);

it("offers the same supported backends as the image configuration", () => {
  expect(
    getAllProviders()
      .map((provider) => provider.id)
      .sort(),
  ).toEqual([...CODESPACE_PROVIDERS].sort());
  const { container } = render(<ProvidersSettings />);
  expect(
    [...container.querySelectorAll('[data-settings-item^="provider-"]')].map(
      (item) => item.getAttribute("data-settings-item"),
    ),
  ).toEqual(["provider-opencode"]);
});
