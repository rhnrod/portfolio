import { animate, stagger } from "https://cdn.jsdelivr.net/npm/motion@12/+esm";

const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

let running = [];
let searchTimer = null;
let navController = null;

function run(anim, el, resetOrigin) {
    const entry = { anim, el, resetOrigin };
    running.push(entry);
    anim.finished
        .then(() => {
            if (el && resetOrigin) el.style.transformOrigin = "";
            const i = running.indexOf(entry);
            if (i !== -1) running.splice(i, 1);
        })
        .catch(() => {});
}

function stopAll() {
    running.forEach(({ anim, el, resetOrigin }) => {
        anim.stop();
        if (el) el.style.transform = "";
        if (resetOrigin) el.style.transformOrigin = "";
    });
    running.length = 0;
}

function isReadView(main) {
    return main.classList.contains("container-read");
}

function scrollToHash(id) {
    const target = document.getElementById(id);
    if (!target) return;
    const scroller = target.closest(".post-content");
    if (!scroller) {
        target.scrollIntoView({ behavior: reduceMotion ? "auto" : "smooth" });
        return;
    }
    const top = target.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop;
    scroller.scrollTo({ top, behavior: reduceMotion ? "auto" : "smooth" });
}

function measureAside(aside) {
    const avatar = aside.querySelector("#avatar");
    const items = Array.prototype.slice.call(aside.querySelectorAll(".navbar li"));
    const iconsRow = aside.querySelector(".icons-row");
    const lastFm = aside.querySelector(".last-fm");
    return {
        avatar: avatar ? avatar.getBoundingClientRect() : null,
        items: items.map((li) => li.getBoundingClientRect()),
        iconsRow: iconsRow ? iconsRow.getBoundingClientRect() : null,
        lastFm: lastFm ? lastFm.getBoundingClientRect() : null,
    };
}

function flipAvatar(el, before, after) {
    const dx = before.left - after.left;
    const dy = before.top - after.top;
    const sx = before.width / after.width;
    const sy = before.height / after.height;
    el.style.transformOrigin = "top left";
    const anim = animate(
        el,
        { x: [dx, 0], y: [dy, 0], scaleX: [sx, 1], scaleY: [sy, 1] },
        { type: "spring", stiffness: 80, damping: 16 }
    );
    run(anim, el, true);
    return anim;
}

function flipItem(el, before, after) {
    const anim = animate(
        el,
        {
            x: [before.left - after.left, 0],
            y: [before.top - after.top, 0],
        },
        { type: "spring", stiffness: 80, damping: 16, delay: stagger(0.06) }
    );
    run(anim, el, false);
    return anim;
}

async function navigate(path, { push = true, force = false } = {}) {
    if (!force && path === location.pathname + location.search + location.hash) return;

    clearTimeout(searchTimer);
    if (navController) navController.abort();
    navController = new AbortController();

    const main = document.querySelector("main");
    if (!main) {
        location.href = path;
        return;
    }

    let res;
    try {
        res = await fetch(path, { headers: { Accept: "text/html" }, signal: navController.signal });
    } catch (err) {
        if (err.name === "AbortError") return;
        location.href = path;
        return;
    }
    if (!res.ok) {
        location.href = path;
        return;
    }
    navController = null;

    const html = await res.text();
    const doc = new DOMParser().parseFromString(html, "text/html");
    const newMain = doc.querySelector("main");
    const newContent = newMain && newMain.querySelector(":scope > div");
    if (!newMain || !newContent) {
        location.href = path;
        return;
    }

    const wasRead = isReadView(main);
    const nowRead = isReadView(newMain);
    const aside = main.querySelector("aside");

    stopAll();

    const before = aside ? measureAside(aside) : null;

    const contentDiv = main.querySelector(":scope > div");
    main.className = newMain.className;
    contentDiv.replaceWith(document.importNode(newContent, true));
    document.title = doc.title || document.title;

    // FLIP só quando atravessa home <-> post. Post->post e home->home trocam instantâneo.
    if (!reduceMotion && aside && before && nowRead !== wasRead) {
        const after = measureAside(aside);
        if (after.avatar && before.avatar) {
            flipAvatar(aside.querySelector("#avatar"), before.avatar, after.avatar);
        }
        const items = Array.prototype.slice.call(aside.querySelectorAll(".navbar li"));
        items.forEach((li, i) => {
            if (before.items[i] && after.items[i]) {
                flipItem(li, before.items[i], after.items[i]);
            }
        });

        const iconsRow = aside.querySelector(".icons-row");
        if (iconsRow && before.iconsRow && after.iconsRow) {
            flipItem(iconsRow, before.iconsRow, after.iconsRow);
        }

        const lastFm = aside.querySelector(".last-fm");
        if (lastFm && before.lastFm && after.lastFm) {
            flipItem(lastFm, before.lastFm, after.lastFm);
        }
    }

    if (push) history.pushState({ path }, "", path);
    window.scrollTo(0, 0);

    if (window.initPostPage) window.initPostPage();

    const hash = location.hash;
    if (hash && hash.length > 1) {
        scrollToHash(hash.slice(1));
    }
}

document.addEventListener("click", (e) => {
    if (e.defaultPrevented || e.button !== 0) return;
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;

    const a = e.target.closest ? e.target.closest("a") : null;
    if (!a) return;
    if (a.target && a.target !== "_self") return;
    if (a.hasAttribute("download")) return;

    const href = a.getAttribute("href");
    if (href === null) return;

    // Âncora interna (#foo): o fragment scroll nativo não rola o scroller
    // aninhado quando o root está travado em 100vh (e trava o container),
    // então rolamos manualmente e só atualizamos o hash via replaceState.
    if (href.charAt(0) === "#") {
        e.preventDefault();
        const id = href.slice(1);
        if (id) {
            scrollToHash(id);
            history.replaceState(null, "", href);
        }
        return;
    }

    // Placeholders (Projetos, #100DaysOfGo): sem navegação
    if (href === "") {
        e.preventDefault();
        return;
    }

    let url;
    try {
        url = new URL(href, location.href);
    } catch (err) {
        return;
    }

    if (url.origin !== location.origin) return;
    if (url.protocol !== "http:" && url.protocol !== "https:") return;

    e.preventDefault();
    navigate(url.pathname + url.search + url.hash, { push: true });
});

document.addEventListener("submit", (e) => {
    const form = e.target.closest ? e.target.closest(".blog-search") : null;
    if (!form) return;
    e.preventDefault();
    const url = new URL(form.action || location.pathname, location.href);
    url.search = new URLSearchParams(new FormData(form)).toString();
    navigate(url.pathname + url.search, { push: true });
});

document.addEventListener("input", (e) => {
    const input = e.target.closest ? e.target.closest(".blog-search input[name=q]") : null;
    const form = input && input.form;
    if (!input || !form) return;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => {
        const caret = input.selectionStart ?? input.value.length;
        const url = new URL(form.action || location.pathname, location.href);
        url.search = new URLSearchParams(new FormData(form)).toString();
        navigate(url.pathname + url.search, { push: true }).then(() => {
            const next = document.querySelector(".blog-search input[name=q]");
            if (!next) return;
            next.focus({ preventScroll: true });
            const pos = Math.min(caret, next.value.length);
            next.setSelectionRange(pos, pos);
        });
    }, 200);
});

window.addEventListener("popstate", () => {
    navigate(location.pathname, { push: false, force: true });
});

if ("scrollRestoration" in history) history.scrollRestoration = "manual";

const initialHash = location.hash;
if (initialHash) {
    const target = document.getElementById(initialHash.slice(1));
    if (target) {
        requestAnimationFrame(() => requestAnimationFrame(() => scrollToHash(initialHash.slice(1))));
    }
}
