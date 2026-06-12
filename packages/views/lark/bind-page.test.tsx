import { describe, expect, it, vi, beforeEach } from "vitest";
import { render, screen, fireEvent, waitFor } from "@testing-library/react";
import { LarkBindPage } from "./bind-page";

const { pushMock, redeemMock, authState } = vi.hoisted(() => ({
  pushMock: vi.fn(),
  redeemMock: vi.fn(),
  authState: { user: null as null | { id: string } },
}));

vi.mock("@multica/core/api", () => ({
  api: { redeemLarkBindingToken: redeemMock },
}));

vi.mock("@multica/core/auth", () => {
  const useAuthStore = (selector: (s: { user: { id: string } | null }) => unknown) =>
    selector({ user: authState.user });
  return { useAuthStore: Object.assign(useAuthStore, { getState: () => ({ user: authState.user }) }) };
});

vi.mock("../navigation", () => ({
  useNavigation: () => ({
    push: pushMock,
    replace: vi.fn(),
    back: vi.fn(),
    pathname: "/lark/bind",
    searchParams: new URLSearchParams(),
    openInNewTab: vi.fn(),
    getShareableUrl: (p: string) => p,
  }),
}));

vi.mock("../i18n", () => ({
  useT: () => ({
    // Return a stable key-ish string so tests can find elements by text.
    t: (sel: (k: Record<string, unknown>) => unknown) => {
      const keys: string[] = [];
      const proxy: Record<string, unknown> = new Proxy(
        {},
        {
          get(_t, prop: string) {
            keys.push(prop);
            return proxy;
          },
        },
      );
      sel({ lark_bind: proxy } as never);
      return keys.join(".");
    },
  }),
}));

describe("LarkBindPage", () => {
  beforeEach(() => {
    pushMock.mockReset();
    redeemMock.mockReset();
    authState.user = null;
  });

  // Regression for the param-name mismatch that silently dropped the
  // binding token: the page sent /login?redirect=… while the login page
  // only honors ?next=. The user logged in, landed on the workspace
  // home, and the token was never redeemed. The redirect target must
  // use `next` — the only param sanitizeNextUrl-backed login reads.
  it("sends logged-out users to /login with the bind URL in ?next=", () => {
    render(<LarkBindPage token="tok123" />);

    fireEvent.click(screen.getByRole("button"));

    expect(pushMock).toHaveBeenCalledTimes(1);
    const target = pushMock.mock.calls[0][0] as string;
    expect(target).toBe(
      `/login?next=${encodeURIComponent("/lark/bind?token=tok123")}`,
    );
  });

  it("auto-redeems when the user is already logged in", async () => {
    authState.user = { id: "user-1" };
    redeemMock.mockResolvedValue({ workspace_id: "ws", installation_id: "inst" });

    render(<LarkBindPage token="tok123" />);

    await waitFor(() => expect(redeemMock).toHaveBeenCalledWith("tok123"));
  });

  it("shows the missing-token error without calling the API", () => {
    render(<LarkBindPage token={null} />);

    expect(redeemMock).not.toHaveBeenCalled();
    expect(screen.getByText(/error_missing_token/)).toBeTruthy();
  });
});
