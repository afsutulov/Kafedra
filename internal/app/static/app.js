(function () {
  'use strict';

  // Версия для слабовидящих: признак хранится в браузере пользователя.
  var root = document.documentElement;
  try { if (localStorage.getItem('a11y') === '1') root.classList.add('a11y'); } catch (e) {}
  var a11y = document.getElementById('a11y-toggle');
  if (a11y) a11y.addEventListener('click', function () {
    var on = root.classList.toggle('a11y');
    try { localStorage.setItem('a11y', on ? '1' : '0'); } catch (e) {}
  });

  // Подтверждение необратимых действий.
  document.addEventListener('submit', function (ev) {
    var msg = ev.target.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) ev.preventDefault();
  });

  // Панель разметки редактора лекций.
  function wrap(area, before, after, placeholder) {
    var s = area.selectionStart, e = area.selectionEnd, v = area.value;
    var sel = v.slice(s, e) || placeholder;
    area.value = v.slice(0, s) + before + sel + after + v.slice(e);
    area.focus();
    area.selectionStart = s + before.length;
    area.selectionEnd = s + before.length + sel.length;
  }
  var marks = {
    bold: ['**', '**', 'текст'], italic: ['*', '*', 'текст'], h: ['\n## ', '\n', 'Заголовок'],
    ul: ['\n- ', '\n', 'пункт'], ol: ['\n1. ', '\n', 'пункт'], code: ['\n```\n', '\n```\n', 'код'],
    link: ['[', '](https://)', 'текст ссылки'], quote: ['\n> ', '\n', 'цитата'],
    table: ['\n| Столбец 1 | Столбец 2 |\n| --- | --- |\n| ', ' |  |\n', 'значение']
  };
  document.addEventListener('click', function (ev) {
    var b = ev.target.closest('[data-md]');
    if (b) {
      var area = document.getElementById(b.getAttribute('data-for') || 'body');
      var m = marks[b.getAttribute('data-md')];
      if (area && m) wrap(area, m[0], m[1], m[2]);
      return;
    }
    var ins = ev.target.closest('[data-insert]');
    if (ins) {
      var ta = document.getElementById('body');
      if (ta) wrap(ta, ins.getAttribute('data-insert'), '', '');
    }
  });

  // Показ полей формы вопроса в зависимости от типа.
  var qtype = document.getElementById('qtype');
  function syncType() {
    var text = qtype.value === 'text';
    document.getElementById('q-options').classList.toggle('hidden', text);
    document.getElementById('q-answers').classList.toggle('hidden', !text);
  }
  if (qtype) { qtype.addEventListener('change', syncType); syncType(); }

  // Тестирование: таймер, автосохранение, автоматическая отправка.
  var form = document.getElementById('attempt-form');
  if (form) {
    var saveUrl = form.getAttribute('data-save');
    var status = document.getElementById('save-status');
    var dirty = false;
    form.addEventListener('change', function () { dirty = true; });
    form.addEventListener('input', function () { dirty = true; });
    var save = function () {
      if (!dirty) return;
      dirty = false;
      fetch(saveUrl, { method: 'POST', body: new URLSearchParams(new FormData(form)), credentials: 'same-origin' })
        .then(function (r) { if (status) status.textContent = r.ok ? 'Ответы сохранены ' + new Date().toLocaleTimeString() : 'Не удалось сохранить ответы'; })
        .catch(function () { dirty = true; if (status) status.textContent = 'Нет связи с сервером — ответы будут сохранены при восстановлении'; });
    };
    setInterval(save, 20000);
    var timer = document.getElementById('timer');
    if (timer) {
      var end = Date.now() + parseInt(timer.getAttribute('data-remaining'), 10) * 1000;
      var tick = function () {
        var left = Math.max(0, Math.round((end - Date.now()) / 1000));
        var m = Math.floor(left / 60), s = left % 60;
        timer.textContent = 'Осталось ' + m + ':' + (s < 10 ? '0' : '') + s;
        timer.classList.toggle('hot', left < 60);
        if (left <= 0) { clearInterval(iv); form.removeAttribute('data-confirm'); form.submit(); }
      };
      var iv = setInterval(tick, 500);
      tick();
    }
  }
})();
