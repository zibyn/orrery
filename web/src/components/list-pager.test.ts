import { describe, expect, it } from 'vitest'

import { PAGE_SIZE, pageSearch, paginate } from './list-pager'

describe('paginate', () => {
  const items = Array.from({ length: PAGE_SIZE * 2 + 3 }, (_, i) => i)

  it('slices the requested page', () => {
    expect(paginate(items, 2)).toMatchObject({ page: 2, pageCount: 3 })
    expect(paginate(items, 2).items[0]).toBe(PAGE_SIZE)
    expect(paginate(items, 3).items).toHaveLength(3)
  })

  it('clamps a page past the end, e.g. after rows were deleted', () => {
    expect(paginate(items, 9).page).toBe(3)
  })

  it('has one empty page for an empty list', () => {
    expect(paginate([], undefined)).toEqual({ items: [], page: 1, pageCount: 1 })
  })
})

describe('pageSearch', () => {
  it('keeps only whole pages above 1', () => {
    expect(pageSearch({ page: 3 })).toEqual({ page: 3 })
    expect(pageSearch({ page: '2' })).toEqual({ page: 2 })
    expect(pageSearch({ page: 1 })).toEqual({ page: undefined })
    expect(pageSearch({ page: 'x' })).toEqual({ page: undefined })
    expect(pageSearch({ page: 1.5 })).toEqual({ page: undefined })
  })
})
