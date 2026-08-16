function worldEasterEgg() {

    const worldEl = document.getElementById('world');
    const chancesAre = Math.floor(Math.random() * 101)

    if (chancesAre <= 5) {
        worldEl.innerHTML = '世界'
    } else {
        worldEl.innerHTML = 'Mundo'
    }
}

worldEasterEgg()