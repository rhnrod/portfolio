import { animate, stagger } from "https://cdn.jsdelivr.net/npm/motion@12/+esm";

const reduceMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;

let running = [];

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
    if (target) target.scrollIntoView({ behavior: reduceMotion ? "auto" : "smooth" });
}

function measureAside(aside) {
    const avatar = aside.querySelector("#avatar");
    const items = Array.prototype.slice.call(aside.querySelectorAll(".navbar li"));
    return {
        avatar: avatar ? avatar.getBoundingClientRect() : null,
        items: items.map((li) => li.getBoundingClientRect()),
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
    if (!force && path === location.pathname) return;

    const main = document.querySelector("main");
    if (!main) {
        location.href = path;
        return;
    }

    let res;
    try {
        res = await fetch(path, { headers: { Accept: "text/html" } });
    } catch (err) {
        location.href = path;
        return;
    }
    if (!res.ok) {
        location.href = path;
        return;
    }

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
